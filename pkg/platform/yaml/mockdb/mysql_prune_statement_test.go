package mockdb

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func TestUpdateMocks_PreservesMySQLPreparedStatementLifecycle(t *testing.T) {
	formats := []struct {
		name   string
		format yaml.Format
		isGob  bool
	}{
		{name: "yaml", format: yaml.FormatYAML, isGob: false},
		{name: "json", format: yaml.FormatJSON, isGob: false},
		{name: "gob", format: yaml.FormatYAML, isGob: true},
	}

	for _, tc := range formats {
		t.Run(tc.name, func(t *testing.T) {
			if tc.isGob {
				t.Setenv("KEPLOY_MOCK_FORMAT", "gob")
			} else {
				t.Setenv("KEPLOY_MOCK_FORMAT", "")
			}

			dir := t.TempDir()
			ys := NewWithFormat(zap.NewNop(), dir, "mocks", tc.format)

			t0 := time.Unix(1_700_000_000, 0).UTC()
			const testSet = "test-set-0"

			// Statement 77 on conn-1: kept via consumed exec77
			prep77 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-1"},
					ReqTimestampMock: t0,
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
							Message: &mysql.StmtPreparePacket{Command: 0x16, Query: "SELECT ? FROM orders"},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
							Message: &mysql.StmtPrepareOkPacket{StatementID: 77},
						},
					}},
				},
			}
			exec77 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-1"},
					ReqTimestampMock: t0.Add(time.Second),
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
							Message: &mysql.StmtExecutePacket{StatementID: 77},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "OK_PACKET"},
							Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
						},
					}},
				},
			}
			close77 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-1"},
					ReqTimestampMock: t0.Add(2 * time.Second),
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_CLOSE"},
							Message: &mysql.StmtClosePacket{StatementID: 77},
						},
					}},
				},
			}

			// Statement 77 on conn-2: different connection, unconsumed, must be pruned
			prep77Conn2 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-2"},
					ReqTimestampMock: t0.Add(3 * time.Second),
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
							Message: &mysql.StmtPreparePacket{Command: 0x16, Query: "SELECT ? FROM orders"},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
							Message: &mysql.StmtPrepareOkPacket{StatementID: 77},
						},
					}},
				},
			}

			// Statement 88 on conn-1: completely unconsumed, must be pruned
			prep88 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-1"},
					ReqTimestampMock: t0.Add(4 * time.Second),
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
							Message: &mysql.StmtPreparePacket{Command: 0x16, Query: "SELECT ? FROM customers"},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
							Message: &mysql.StmtPrepareOkPacket{StatementID: 88},
						},
					}},
				},
			}
			exec88 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-1"},
					ReqTimestampMock: t0.Add(5 * time.Second),
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
							Message: &mysql.StmtExecutePacket{StatementID: 88},
						},
					}},
					MySQLResponses: []mysql.Response{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "OK_PACKET"},
							Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
						},
					}},
				},
			}
			close88 := &models.Mock{
				Version: "api.keploy.io/v1beta1",
				Kind:    models.MySQL,
				Spec: models.MockSpec{
					Metadata:         map[string]string{"connID": "conn-1"},
					ReqTimestampMock: t0.Add(6 * time.Second),
					MySQLRequests: []mysql.Request{{
						PacketBundle: mysql.PacketBundle{
							Header:  &mysql.PacketInfo{Type: "COM_STMT_CLOSE"},
							Message: &mysql.StmtClosePacket{StatementID: 88},
						},
					}},
				},
			}

			allMocks := []*models.Mock{prep77, exec77, close77, prep77Conn2, prep88, exec88, close88}
			ctx := context.Background()
			for _, m := range allMocks {
				if err := ys.InsertMock(ctx, m, testSet); err != nil {
					t.Fatalf("InsertMock(%s): %v", m.Name, err)
				}
			}
			if err := ys.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			// Only exec77 is consumed by test run
			consumed := map[string]models.MockState{
				exec77.Name: {Name: exec77.Name, Kind: models.MySQL},
			}
			pruneBefore := t0.Add(10 * time.Minute)

			// Re-open mockdb to update mocks
			ysPrune := NewWithFormat(zap.NewNop(), dir, "mocks", tc.format)
			if err := ysPrune.UpdateMocks(ctx, testSet, consumed, pruneBefore, time.Time{}); err != nil {
				t.Fatalf("UpdateMocks: %v", err)
			}

			// Read back surviving mocks
			var survivingNames []string
			if tc.isGob {
				mks, err := readGobMocks(filepath.Join(dir, testSet, "mocks.gob"))
				if err != nil {
					t.Fatalf("readGobMocks: %v", err)
				}
				for _, m := range mks {
					survivingNames = append(survivingNames, m.Name)
				}
			} else {
				set, err := ysPrune.GetTestSetMocks(ctx, testSet, models.BaseTime, time.Now(), nil, nil)
				if err != nil {
					t.Fatalf("GetTestSetMocks: %v", err)
				}
				for _, m := range append(set.AllPerTest, set.AllSession...) {
					survivingNames = append(survivingNames, m.Name)
				}
			}

			// exec77, prep77, and close77 must survive
			for _, keptMock := range []*models.Mock{prep77, exec77, close77} {
				if !slices.Contains(survivingNames, keptMock.Name) {
					t.Errorf("expected mock %s (statement 77, conn-1) to survive prune, but it was pruned. Surviving: %v", keptMock.Name, survivingNames)
				}
			}

			// prep77Conn2, prep88, exec88, close88 must be pruned
			for _, prunedMock := range []*models.Mock{prep77Conn2, prep88, exec88, close88} {
				if slices.Contains(survivingNames, prunedMock.Name) {
					t.Errorf("expected mock %s to be pruned, but it survived. Surviving: %v", prunedMock.Name, survivingNames)
				}
			}
		})
	}
}

func TestUpdateMocks_StatementIDReuseAcrossConnectionsAndQueries(t *testing.T) {
	dir := t.TempDir()
	ys := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	t0 := time.Unix(1_700_000_000, 0).UTC()
	const testSet = "test-set-0"
	ctx := context.Background()

	// conn-1: stmt 1 query A (kept)
	c1Prep1A := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "conn-1"},
			ReqTimestampMock: t0,
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
					Message: &mysql.StmtPreparePacket{Command: 0x16, Query: "SELECT 1"},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
					Message: &mysql.StmtPrepareOkPacket{StatementID: 1},
				},
			}},
		},
	}
	c1Exec1A := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "conn-1"},
			ReqTimestampMock: t0.Add(time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
					Message: &mysql.StmtExecutePacket{StatementID: 1},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "OK"},
					Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
				},
			}},
		},
	}

	// conn-2: stmt 1 query B (kept)
	c2Prep1 := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "conn-2"},
			ReqTimestampMock: t0.Add(2 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
					Message: &mysql.StmtPreparePacket{Command: 0x16, Query: "SELECT 2"},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
					Message: &mysql.StmtPrepareOkPacket{StatementID: 1},
				},
			}},
		},
	}
	c2Exec1 := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "conn-2"},
			ReqTimestampMock: t0.Add(3 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
					Message: &mysql.StmtExecutePacket{StatementID: 1},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "OK"},
					Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
				},
			}},
		},
	}

	// conn-3: stmt 1 query C (NOT kept / unconsumed)
	c3Prep1 := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "conn-3"},
			ReqTimestampMock: t0.Add(4 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE"},
					Message: &mysql.StmtPreparePacket{Command: 0x16, Query: "SELECT 3"},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_PREPARE_OK"},
					Message: &mysql.StmtPrepareOkPacket{StatementID: 1},
				},
			}},
		},
	}
	c3Exec1 := &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.MySQL,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"connID": "conn-3"},
			ReqTimestampMock: t0.Add(5 * time.Second),
			MySQLRequests: []mysql.Request{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
					Message: &mysql.StmtExecutePacket{StatementID: 1},
				},
			}},
			MySQLResponses: []mysql.Response{{
				PacketBundle: mysql.PacketBundle{
					Header:  &mysql.PacketInfo{Type: "OK"},
					Message: &mysql.OKPacket{Header: 0x00, AffectedRows: 1},
				},
			}},
		},
	}

	for _, m := range []*models.Mock{c1Prep1A, c1Exec1A, c2Prep1, c2Exec1, c3Prep1, c3Exec1} {
		if err := ys.InsertMock(ctx, m, testSet); err != nil {
			t.Fatalf("InsertMock(%s): %v", m.Name, err)
		}
	}
	if err := ys.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Consume conn-1's execution and conn-2's execution; conn-3 is unconsumed
	consumed := map[string]models.MockState{
		c1Exec1A.Name: {Name: c1Exec1A.Name, Kind: models.MySQL},
		c2Exec1.Name:  {Name: c2Exec1.Name, Kind: models.MySQL},
	}

	ysPrune := NewWithFormat(zap.NewNop(), dir, "mocks", yaml.FormatYAML)
	if err := ysPrune.UpdateMocks(ctx, testSet, consumed, t0.Add(time.Hour), time.Time{}); err != nil {
		t.Fatalf("UpdateMocks: %v", err)
	}

	set, err := ysPrune.GetTestSetMocks(ctx, testSet, models.BaseTime, time.Now(), nil, nil)
	if err != nil {
		t.Fatalf("GetTestSetMocks: %v", err)
	}
	var surviving []string
	for _, m := range append(set.AllPerTest, set.AllSession...) {
		surviving = append(surviving, m.Name)
	}

	// conn-1's prep and exec survive
	if !slices.Contains(surviving, c1Prep1A.Name) || !slices.Contains(surviving, c1Exec1A.Name) {
		t.Errorf("conn-1 statement 1 should survive: got %v", surviving)
	}
	// conn-2's prep and exec survive
	if !slices.Contains(surviving, c2Prep1.Name) || !slices.Contains(surviving, c2Exec1.Name) {
		t.Errorf("conn-2 statement 1 should survive: got %v", surviving)
	}
	// conn-3's prep and exec MUST be pruned
	if slices.Contains(surviving, c3Prep1.Name) || slices.Contains(surviving, c3Exec1.Name) {
		t.Errorf("conn-3 statement 1 should be pruned: got %v", surviving)
	}
}
