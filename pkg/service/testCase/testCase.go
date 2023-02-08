package testCase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"

	grpcMock "go.keploy.io/server/grpc/mock"
	proto "go.keploy.io/server/grpc/regression"
	"go.keploy.io/server/grpc/utils"
	"go.keploy.io/server/pkg"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/platform/telemetry"
	"go.uber.org/zap"
)

func New(tdb models.TestCaseDB, log *zap.Logger, EnableDeDup bool, adb telemetry.Service, client http.Client, TestExport bool, mFS models.MockFS) *TestCase {
	return &TestCase{
		tdb:           tdb,
		tele:          adb,
		log:           log,
		mockFS:        mFS,
		testExport:    TestExport,
		client:        client,
		mu:            sync.Mutex{},
		anchors:       map[string][]map[string][]string{},
		noisyFields:   map[string]map[string]bool{},
		fieldCounts:   map[string]map[string]map[string]int{},
		EnableDeDup:   EnableDeDup,
		nextYamlIndex: yamlTcsIndx{tcsCount: map[string]int{}, mu: sync.Mutex{}},
	}
}

type yamlTcsIndx struct {
	tcsCount map[string]int // number of testcases with app_id
	mu       sync.Mutex
}

type TestCase struct {
	tdb           models.TestCaseDB
	tele          telemetry.Service
	mockFS        models.MockFS
	testExport    bool
	client        http.Client
	log           *zap.Logger
	nextYamlIndex yamlTcsIndx
	mu            sync.Mutex
	appCount      int
	// index is `cid-appID-uri`
	//
	// anchors is map[index][]map[key][]value or map[index]combinationOfAnchors
	// anchors stores all the combinations of anchor fields for a particular index
	// anchor field is a low variance field which is used in the deduplication algorithm.
	// example: user-type or blood-group could be good anchor fields whereas timestamps
	// and usernames are bad anchor fields.
	// during deduplication only anchor fields are compared for new requests to determine whether it is a duplicate or not.
	// other fields are ignored.
	anchors map[string][]map[string][]string
	// noisyFields is map[index][key]bool
	noisyFields map[string]map[string]bool
	// fieldCounts is map[index][key][value]count
	// fieldCounts stores the count of all values of a particular field in an index.
	// eg: lets say field is bloodGroup then the value would be {A+: 20, B+: 10,...}
	fieldCounts map[string]map[string]map[string]int
	EnableDeDup bool
}

func (r *TestCase) Delete(ctx context.Context, cid, id string) error {
	// reset cache
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.tdb.Get(ctx, cid, id)
	if err != nil {

		// r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.Error(err))
		pkg.LogError("failed to get testcases from the DB", r.log, err, map[string]interface{}{"cid": cid, "id": id})
		return errors.New("internal failure")
	}
	index := fmt.Sprintf("%s-%s-%s", t.CID, t.AppID, t.URI)
	delete(r.anchors, index)
	err = r.tdb.Delete(ctx, id)
	if err != nil {
		pkg.LogError("failed to delete testcases from the DB", r.log, err, map[string]interface{}{"cid": cid, "id": id})
		// r.log.Error("failed to delete testcase from the DB", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
		return errors.New("internal failure")
	}

	r.tele.DeleteTc(r.client, ctx)
	return nil
}

func (r *TestCase) GetApps(ctx context.Context, cid string) ([]string, error) {
	apps, err := r.tdb.GetApps(ctx, cid)
	if apps != nil && len(apps) != r.appCount {
		r.tele.GetApps(len(apps), r.client, ctx)
		r.appCount = len(apps)
	}
	return apps, err
}

// Get returns testcase with specific company_id, app_id and id.
//
// Note: During testcase-export, generated testcase will not be displayed in ui currently. Because path is not provided by the ui graphQL query.
func (r *TestCase) Get(ctx context.Context, cid, appID, id string) (models.TestCase, error) {
	if r.testExport {
		return models.TestCase{}, pkg.LogError("failed to call query call for tcs", r.log, errors.New("keploy is running on test-export. Export `ENABLE_TEST_EXPORT` env variable as 'false'"))
	}
	tcs, err := r.tdb.Get(ctx, cid, id)
	if err != nil {
		// return nil,
		// sanitizedAppID := pkg.SanitiseInput(appID)
		// r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.String("appID", sanitizedAppID), zap.Error(err))
		pkg.LogError("failed to get testcases from the DB", r.log, err, map[string]interface{}{"appID": appID, "cid": cid})
		return models.TestCase{}, errors.New("internal failure")
	}
	return tcs, nil
}

// readTCS returns all the generated testcases and their mocks from the testCasePath and mockPath directory. It returns all the testcases.
func (r *TestCase) readTCS(ctx context.Context, testCasePath, mockPath string) ([]models.TestCase, error) {
	if testCasePath == "" || mockPath == "" || !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		return nil, fmt.Errorf("file path should be absolute. got testcase path: %s and mock path: %s", pkg.SanitiseInput(testCasePath), pkg.SanitiseInput(mockPath))
	}
	res, err := r.mockFS.ReadAll(ctx, testCasePath, mockPath)
	if err != nil {
		r.log.Info(fmt.Sprintf("no testcases found in %s directory.", pkg.SanitiseInput(testCasePath)))
		return nil, err
	}
	return res, err
}

func (r *TestCase) GetAll(ctx context.Context, cid, appID string, offset *int, limit *int, testCasePath, mockPath string) ([]models.TestCase, error) {
	off, lim := 0, 25
	if offset != nil {
		off = *offset
	}
	if limit != nil {
		lim = *limit
	}

	if r.testExport {
		return r.readTCS(ctx, testCasePath, mockPath)
	}

	tcs, err := r.tdb.GetAll(ctx, cid, appID, false, off, lim)

	if err != nil {
		return nil, pkg.LogError("failed to get testcases from the DB", r.log, err, map[string]interface{}{"appID": appID, "cid": cid})
		// sanitizedAppID := pkg.SanitiseInput(appID)
		// r.log.Error("failed to get testcases from the DB", zap.String("cid", cid), zap.String("appID", sanitizedAppID), zap.Error(err))
		// return nil, errors.New("internal failure")
	}
	return tcs, nil
}

func (r *TestCase) Update(ctx context.Context, t []models.TestCase) error {
	for _, v := range t {
		err := r.tdb.UpdateTC(ctx, v)
		if err != nil {
			return pkg.LogError("failed to insert testcase into DB", r.log, err, map[string]interface{}{"appID": v.AppID})
			// r.log.Error("failed to insert testcase into DB", zap.String("appID", v.AppID), zap.Error(err))
			// return errors.New("internal failure")
		}
	}
	r.tele.EditTc(r.client, ctx)
	return nil
}

func (r *TestCase) putTC(ctx context.Context, cid string, t models.TestCase) (string, error) {
	t.CID = cid

	var err error
	if r.EnableDeDup {
		// check if already exists
		dup, err := r.isDup(ctx, &t)
		if err != nil {
			return "", pkg.LogError("failed to run deduplication on the testcase", r.log, err, map[string]interface{}{"appID": t.AppID, "cid": cid})
			// r.log.Error("failed to run deduplication on the testcase", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
			// return "", errors.New("internal failure")
		}
		if dup {
			// TODO: Call the generic info logger func
			r.log.Info("found duplicate testcase", zap.String("cid", cid), zap.String("appID", t.AppID), zap.String("uri", t.URI))
			return "", nil
		}
	}
	err = r.tdb.Upsert(ctx, t)
	if err != nil {
		return "", pkg.LogError("failed to insert testcase into DB", r.log, err, map[string]interface{}{"cid": cid, "appID": t.AppID})
		// r.log.Error("failed to insert testcase into DB", zap.String("cid", cid), zap.String("appID", t.AppID), zap.Error(err))
		// return "", errors.New("internal failure")
	}

	return t.ID, nil
}

func (r *TestCase) insertToDB(ctx context.Context, cid string, tcs []models.TestCase) ([]string, error) {
	var ids []string
	if len(tcs) == 0 {
		return nil, pkg.LogError("no testcase to update", r.log, errors.New("no testcase to update"))
		// err := errors.New("no testcase to update")
		// r.log.Error(err.Error())
		// return nil, err

	}
	for _, t := range tcs {
		id, err := r.putTC(ctx, cid, t)
		if err != nil {
			// msg := "failed saving testcase"
			return nil, pkg.LogError("failed saving testcase", r.log, err, map[string]interface{}{"id": t.ID, "appID": t.AppID, "cid": cid})

			// r.log.Error(msg, zap.Error(err), zap.String("cid", cid), zap.String("id", t.ID), zap.String("app", t.AppID))
			// return nil, errors.New(msg)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// writeToYaml Write will write testcases into the path directory as yaml files.
// Note: dedup algo is not executed during testcase-export currently.
func (r *TestCase) writeToYaml(ctx context.Context, test []models.Mock, testCasePath, mockPath string) ([]string, error) {
	if testCasePath == "" || !pkg.IsValidPath(testCasePath) || !pkg.IsValidPath(mockPath) {
		err := fmt.Errorf("path directory not found. got testcase path: %s and mock path: %s", pkg.SanitiseInput(testCasePath), pkg.SanitiseInput(mockPath))
		return nil, pkg.LogError("", r.log, err)
		// r.log.Error(err.Error())
		// return nil, err
	}
	// test[0] will always be a testcase. test[1:] will be the mocks.
	// check for known noisy fields like dates
	err := r.mockFS.Write(ctx, testCasePath, test[0])
	if err != nil {
		return nil, pkg.LogError("", r.log, err)
		// r.log.Error(err.Error())
		// return nil, err
	}
	r.log.Info(fmt.Sprint("\n💾 Recorded testcase with name: ", test[0].Name, " in yaml file at path: ", testCasePath, "\n"))
	mockName := "mock" + test[0].Name[4:]

	if len(test) > 1 {
		err = r.mockFS.WriteAll(ctx, mockPath, mockName, test[1:])
		if err != nil {
			return nil, pkg.LogError("", r.log, err)
			// r.log.Error(err.Error())
			// return nil, err

		}
		r.log.Info(fmt.Sprint("\n💾 Recorded mocks for testcase with name: ", test[0].Name, " at path: ", mockPath, "\n"))
	}
	return []string{test[0].Name}, nil
}

func (r *TestCase) Insert(ctx context.Context, t []models.TestCase, testCasePath, mockPath, cid string) ([]string, error) {
	var (
		inserted = []string{}
		err      error
	)
	for _, v := range t {
		// store testcase in yaml file
		if r.testExport {
			r.nextYamlIndex.mu.Lock()
			// defer r.nextYamlIndex.mu.Unlock()
			lastIndex, ok := r.nextYamlIndex.tcsCount[v.AppID]
			if !ok {
				tcs, err := r.GetAll(ctx, v.CID, v.AppID, nil, nil, testCasePath, mockPath)
				if len(tcs) > 0 && err == nil {
					// filename is of the format "test-<Sequence_Number>.yaml"
					if len(strings.Split(tcs[len(tcs)-1].ID, "-")) < 1 ||
						len(strings.Split(strings.Split(tcs[len(tcs)-1].ID, "-")[1], ".")) == 0 {
						return nil, errors.New("failed to decode the last sequence number from yaml test")
					}
					indx := strings.Split(strings.Split(tcs[len(tcs)-1].ID, "-")[1], ".")[0]
					lastIndex, err = strconv.Atoi(indx)
					if err != nil {
						return nil, pkg.LogError(
							"failed to get the last sequence number for testcase",
							r.log,
							err,
						)
						// r.log.Error("failed to get the last sequence number for testcase", zap.Error(err))
						// return nil, err
					}
				}
			}
			r.nextYamlIndex.tcsCount[v.AppID] = lastIndex + 1
			var (
				id = fmt.Sprintf("test-%v", lastIndex+1)

				tc = []models.Mock{{
					Version: models.V1Beta2,
					Kind:    models.HTTP,
					Name:    id,
				}}
				mocks = []string{}
			)

			for i, j := range v.Mocks {
				doc, err := grpcMock.Encode(j)
				if err != nil {
					return nil, pkg.LogError(
						"failed to encode mocks to write",
						r.log,
						err,
						map[string]interface{}{"test id": doc.Name},
					)
					// r.log.Error(err.Error())
				}
				tc = append(tc, doc)
				m := "mock-" + fmt.Sprint(lastIndex+1) + "-" + strconv.Itoa(i)
				tc[len(tc)-1].Name = m
				mocks = append(mocks, m)
			}

			testcase := &proto.Mock{
				Version: string(models.V1Beta2),
				Name:    id,
				Spec: &proto.Mock_SpecSchema{
					Objects: []*proto.Mock_Object{{
						Type: "error",
						Data: []byte{},
					}},
					Mocks: mocks,
					Assertions: map[string]*proto.StrArr{
						"noise": {Value: v.Noise},
					},
					Created: v.Captured,
				},
			}
			switch v.Type {
			case string(models.HTTP):
				testcase.Kind = string(models.HTTP)
				testcase.Spec.Req = &proto.HttpReq{
					Method:     string(v.HttpReq.Method),
					ProtoMajor: int64(v.HttpReq.ProtoMajor),
					ProtoMinor: int64(v.HttpReq.ProtoMinor),
					URL:        v.HttpReq.URL,
					URLParams:  v.HttpReq.URLParams,
					Body:       v.HttpReq.Body,
					Header:     utils.GetProtoMap(v.HttpReq.Header),
					Form:       grpcMock.GetProtoFormData(v.HttpReq.Form),
				}
				testcase.Spec.Res = &proto.HttpResp{
					StatusCode:    int64(v.HttpResp.StatusCode),
					Body:          v.HttpResp.Body,
					Header:        utils.GetProtoMap(v.HttpResp.Header),
					StatusMessage: v.HttpResp.StatusMessage,
					ProtoMajor:    int64(v.HttpReq.ProtoMajor),
					ProtoMinor:    int64(v.HttpReq.ProtoMinor),
					Binary:        v.HttpResp.Binary,
				}
			case string(models.GRPC_EXPORT):
				testcase.Kind = string(models.GRPC_EXPORT)
				testcase.Spec.GrpcRequest = &proto.GrpcReq{
					Body:   v.GrpcReq.Body,
					Method: v.GrpcReq.Method,
				}
				testcase.Spec.GrpcResp = &proto.GrpcResp{
					Body: v.GrpcResp.Body,
					Err:  v.GrpcResp.Err,
				}
			}
			tcsMock, err := grpcMock.Encode(testcase)
			if err != nil {
				return nil, pkg.LogError("", r.log, err)
				// r.log.Error(err.Error())
				// return nil, err
			}
			tc[0] = tcsMock
			insertedIds, err := r.writeToYaml(ctx, tc, testCasePath, mockPath)
			r.nextYamlIndex.mu.Unlock()
			if err != nil {
				return nil, err
			}
			inserted = append(inserted, insertedIds...)
			continue
		}

		// store testcase in mongoDB
		v.Mocks = nil
		insertedIds, err := r.insertToDB(ctx, cid, []models.TestCase{v})
		if err != nil {
			return nil, err
		}
		inserted = append(inserted, insertedIds...)
	}
	return inserted, err
}

func (r *TestCase) fillCache(ctx context.Context, t *models.TestCase) (string, error) {

	uri := ""
	switch t.Type {
	case string(models.HTTP):
		uri = t.URI
	case string(models.GRPC_EXPORT):
		uri = t.GrpcReq.Method
	}
	index := fmt.Sprintf("%s-%s-%s", t.CID, t.AppID, uri)
	_, ok1 := r.noisyFields[index]
	_, ok2 := r.fieldCounts[index]
	if ok1 && ok2 {
		return index, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// check again after the lock
	_, ok1 = r.noisyFields[index]
	_, ok2 = r.fieldCounts[index]

	if !ok1 || !ok2 {
		var anchors []map[string][]string
		fieldCounts, noisyFields := map[string]map[string]int{}, map[string]bool{}
		tcs, err := r.tdb.GetKeys(ctx, t.CID, t.AppID, uri, t.Type) // TODO: add method for grpc
		if err != nil {
			return "", err
		}
		for _, v := range tcs {
			//var appAnchors map[string][]string
			//for _, a := range v.Anchors {
			//	appAnchors[a] = v.AllKeys[a]
			//}
			anchors = append(anchors, v.Anchors)
			for k, v1 := range v.AllKeys {
				if fieldCounts[k] == nil {
					fieldCounts[k] = map[string]int{}
				}
				for _, v2 := range v1 {
					fieldCounts[k][v2] = fieldCounts[k][v2] + 1
				}
				if !isAnchor(fieldCounts[k]) {
					noisyFields[k] = true
				}
			}
		}
		r.fieldCounts[index], r.noisyFields[index], r.anchors[index] = fieldCounts, noisyFields, anchors
	}
	return index, nil
}

func (r *TestCase) isDup(ctx context.Context, t *models.TestCase) (bool, error) {

	reqKeys := map[string][]string{}
	filterKeys := map[string][]string{}
	uri := ""

	index, err := r.fillCache(ctx, t)
	if err != nil {
		return false, err
	}

	switch t.Type {
	case string(models.HTTP):
		uri = t.URI
		// add headers
		for k, v := range t.HttpReq.Header {
			reqKeys["header."+k] = []string{strings.Join(v, "")}
		}

		// add url params
		for k, v := range t.HttpReq.URLParams {
			reqKeys["url_params."+k] = []string{v}
		}

		// add body if it is a valid json
		if json.Valid([]byte(t.HttpReq.Body)) {
			var result interface{}

			err = json.Unmarshal([]byte(t.HttpReq.Body), &result)
			if err != nil {
				return false, err
			}
			body := pkg.Flatten(result)
			for k, v := range body {
				nk := "body"
				if k != "" {
					nk = nk + "." + k
				}
				reqKeys[nk] = v
			}
		}
	case string(models.GRPC_EXPORT):
		uri = t.GrpcReq.Method
		if json.Valid([]byte(t.GrpcReq.Body)) {
			var result interface{}

			err = json.Unmarshal([]byte(t.GrpcReq.Body), &result)
			if err != nil {
				return false, err
			}
			body := pkg.Flatten(result)
			for k, v := range body {
				nk := "body"
				if k != "" {
					nk = nk + "." + k
				}
				reqKeys[nk] = v
			}
		}
	}

	isAnchorChange := false
	for k, v := range reqKeys {
		if !r.noisyFields[index][k] {
			// update field count
			for _, s := range v {
				if _, ok := r.fieldCounts[index][k]; !ok {
					r.fieldCounts[index][k] = map[string]int{}
				}
				r.fieldCounts[index][k][s] = r.fieldCounts[index][k][s] + 1
			}
			if !isAnchor(r.fieldCounts[index][k]) {
				r.noisyFields[index][k] = true
				isAnchorChange = true
				continue
			}
			filterKeys[k] = v
		}
	}

	if len(filterKeys) == 0 {
		return true, nil
	}
	if isAnchorChange {
		err = r.tdb.DeleteByAnchor(ctx, t.CID, t.AppID, uri, t.Type, filterKeys)
		if err != nil {
			return false, err
		}
	}

	// check if testcase based on anchor keys already exists
	dup, err := r.exists(ctx, filterKeys, index)
	if err != nil {
		return false, err
	}

	t.AllKeys = reqKeys
	//var keys []string
	//for k := range filterKeys {
	//	keys = append(keys, k)
	//}
	t.Anchors = filterKeys
	r.anchors[index] = append(r.anchors[index], filterKeys)

	return dup, nil
}

func (r *TestCase) exists(_ context.Context, anchors map[string][]string, index string) (bool, error) {
	for _, v := range anchors {
		sort.Strings(v)
	}
	for _, v := range r.anchors[index] {
		if reflect.DeepEqual(v, anchors) {
			return true, nil
		}
	}
	return false, nil

}

func isAnchor(m map[string]int) bool {
	totalCount := 0
	for _, v := range m {
		totalCount = totalCount + v
	}
	// if total values for that field is less than 20 then,
	// the sample size is too small to know if its high variance.
	if totalCount < 20 {
		return true
	}
	// if the unique values are less than 40% of the total value count them,
	// the field is low variant.
	if float64(totalCount)*0.40 > float64(len(m)) {
		return true
	}
	return false
}
