package generic

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
)

// fuzzyMatch performs a fuzzy matching algorithm to find the best matching mock for the given request.
// It takes a context, a request buffer, and a mock database as input parameters.
// The function iterates over the mocks in the database and applies the fuzzy matching algorithm to find the best match.
// If a match is found, it returns the corresponding response mock and a boolean value indicating success.
// If no match is found, it returns false and a nil response.
// If an error occurs during the matching process, it returns an error.
func fuzzyMatch(ctx context.Context, logger *zap.Logger, reqBuff [][]byte, mockDb integrations.MockMemDb, fuzzyPolicy string) (bool, []models.Payload, error) {
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:
			mocks, err := mockDb.GetSessionMocks()
			if err != nil {
				return false, nil, fmt.Errorf("error while getting session mocks: %w", err)
			}

			var filteredMocks []*models.Mock
			var unfilteredMocks []*models.Mock

			for _, mock := range mocks {
				if mock.Kind != "Generic" {
					continue
				}
				if mock.TestModeInfo.IsFiltered {
					filteredMocks = append(filteredMocks, mock)
				} else {
					unfilteredMocks = append(unfilteredMocks, mock)
				}
			}

			logger.Debug("List of mocks in the database", zap.Int("Filtered Mocks", len(filteredMocks)), zap.Int("Unfiltered Mocks", len(unfilteredMocks)))
			for i, mock := range filteredMocks {
				logger.Debug("Filtered Mocks", zap.String(fmt.Sprintf("Mock[%d]", i), mock.Name), zap.Int64("sortOrder", mock.TestModeInfo.SortOrder))
			}
			for i, mock := range unfilteredMocks {
				logger.Debug("Unfiltered Mocks", zap.String(fmt.Sprintf("Mock[%d]", i), mock.Name), zap.Int64("sortOrder", mock.TestModeInfo.SortOrder))
			}

			index := findExactMatch(filteredMocks, reqBuff)

			if index == -1 && fuzzyPolicy != models.FuzzyMatchOff {
				index = findBinaryMatch(filteredMocks, reqBuff, 0.9)
				if index != -1 && fuzzyPolicy != models.FuzzyMatchOn {
					logger.Warn("generic mock served via similarity (Jaccard) match — verify this is the right mock or set test.fuzzyMatch=off for deterministic replay",
						zap.String("mock name", filteredMocks[index].Name),
						zap.Float64("threshold", 0.9))
				}
			}

			// Concurrency note for both branches below: see HTTP's
			// updateMock — filteredMocks[index] / unfilteredMocks[index]
			// are pointers into the shared mock pool, so we mutate a
			// fresh copy rather than the pool pointer and pass the copy
			// to UpdateUnFilteredMock.
			if index != -1 {
				responseMock := make([]models.Payload, len(filteredMocks[index].Spec.GenericResponses))
				copy(responseMock, filteredMocks[index].Spec.GenericResponses)
				updatedMock := *filteredMocks[index]
				updatedMock.TestModeInfo.IsFiltered = false
				updatedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
				isUpdated := mockDb.UpdateUnFilteredMock(filteredMocks[index], &updatedMock)
				if !isUpdated {
					continue
				}
				logger.Debug("Filtered mock found for generic request", zap.String("Mock", updatedMock.Name), zap.Int64("sortOrder", updatedMock.TestModeInfo.SortOrder))
				return true, responseMock, nil
			}

			index = findExactMatch(unfilteredMocks, reqBuff)

			if index == -1 && fuzzyPolicy != models.FuzzyMatchOff {
				index = findBinaryMatch(unfilteredMocks, reqBuff, 0.4)
				if index != -1 && fuzzyPolicy != models.FuzzyMatchOn {
					logger.Warn("generic mock served via similarity (Jaccard) match — verify this is the right mock or set test.fuzzyMatch=off for deterministic replay",
						zap.String("mock name", unfilteredMocks[index].Name),
						zap.Float64("threshold", 0.4))
				}
			}
			if index != -1 {
				responseMock := make([]models.Payload, len(unfilteredMocks[index].Spec.GenericResponses))
				copy(responseMock, unfilteredMocks[index].Spec.GenericResponses)
				updatedMock := *unfilteredMocks[index]
				updatedMock.TestModeInfo.IsFiltered = false
				updatedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
				isUpdated := mockDb.UpdateUnFilteredMock(unfilteredMocks[index], &updatedMock)
				if !isUpdated {
					continue
				}
				logger.Debug("Unfiltered mock found for generic request", zap.String("Mock", updatedMock.Name), zap.Int64("sortOrder", updatedMock.TestModeInfo.SortOrder))
				return true, responseMock, nil
			}
			return false, nil, nil
		}
	}
}

// buildGenericMismatchReport builds the universal mock-miss report for the
// generic (opaque TCP) parser. Field-level diffs are impossible for unparsed
// wire bytes, so the report carries the closest candidate by Jaccard
// similarity with its score — enough for the user to tell "nothing remotely
// similar was recorded" (re-record) apart from "a near-miss exists"
// (protocol drift, likely needs re-record of just this dependency).
func buildGenericMismatchReport(ctx context.Context, reqBuffs [][]byte, mockDb integrations.MockMemDb) *models.MockMismatchReport {
	summary := fmt.Sprintf("opaque TCP exchange (%d request buffer(s), first %d bytes)", len(reqBuffs), func() int {
		if len(reqBuffs) == 0 {
			return 0
		}
		return len(reqBuffs[0])
	}())

	mocks, err := mockDb.GetSessionMocks()
	if err != nil || ctx.Err() != nil {
		return mismatch.NewReport(mismatch.ProtocolGeneric, summary).Build()
	}
	var genericMocks []*models.Mock
	for _, m := range mocks {
		if m.Kind == "Generic" {
			genericMocks = append(genericMocks, m)
		}
	}
	if len(genericMocks) == 0 {
		return mismatch.NewReport(mismatch.ProtocolGeneric, summary).
			WithPhase(models.MatchPhaseNoMocks, 0).Build()
	}

	bestIdx, bestSim := -1, -1.0
	for idx, mock := range genericMocks {
		if len(mock.Spec.GenericRequests) != len(reqBuffs) {
			continue
		}
		var simSum float64
		comparable := true
		for i, reqBuff := range reqBuffs {
			msg := mock.Spec.GenericRequests[i].Message[0]
			// The recorder stores ASCII payloads verbatim (Type String) and
			// binary payloads base64-encoded — decode per the recorded type,
			// otherwise the similarity is computed against nil bytes and the
			// closest-candidate ranking degrades to noise.
			var recorded []byte
			if msg.Type == models.String {
				recorded = []byte(msg.Data)
			} else {
				decoded, err := util.DecodeBase64(msg.Data)
				if err != nil {
					comparable = false
					break
				}
				recorded = decoded
			}
			simSum += fuzzyCheck(recorded, reqBuff)
		}
		if !comparable {
			continue
		}
		if avg := simSum / float64(len(reqBuffs)); avg > bestSim {
			bestSim = avg
			bestIdx = idx
		}
	}

	b := mismatch.NewReport(mismatch.ProtocolGeneric, summary).
		WithPhase(models.MatchPhaseExhausted, len(genericMocks))
	if bestIdx >= 0 {
		b = b.WithClosest(genericMocks[bestIdx].Name, nil).
			WithDiff(fmt.Sprintf("closest recorded exchange %q has Jaccard similarity %.2f (thresholds: 0.9 per-test, 0.4 session)", genericMocks[bestIdx].Name, bestSim))
	} else {
		b = b.WithDiff("no recorded exchange has the same number of request buffers")
	}
	return b.Build()
}

// TODO: need to generalize this function for different types of integrations.
func findBinaryMatch(tcsMocks []*models.Mock, reqBuffs [][]byte, mxSim float64) int {
	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(reqBuffs) {
			nc := util.NewNoiseChecker(mock.Noise)
			var simSum float64
			var simCount int
			for requestIndex, reqBuff := range reqBuffs {
				mockData := mock.Spec.GenericRequests[requestIndex].Message[0].Data

				// Skip noisy (obfuscated) buffers — don't let them influence similarity
				if nc != nil && nc.IsNoisy(mockData) {
					continue
				}

				encoded, _ := util.DecodeBase64(mockData)

				similarity := fuzzyCheck(encoded, reqBuff)
				simSum += similarity
				simCount++
			}
			// Compute average similarity across non-noisy buffers.
			// If all buffers are noisy, treat as neutral (1.0) so the
			// mock remains matchable — schema/length already matched.
			if simCount > 0 {
				avgSim := simSum / float64(simCount)
				if avgSim > mxSim {
					mxSim = avgSim
					mxIdx = idx
				}
			} else if len(reqBuffs) > 0 {
				// All buffers were noisy — neutral match
				if 1.0 > mxSim {
					mxSim = 1.0
					mxIdx = idx
				}
			}
		}
	}
	return mxIdx
}

func fuzzyCheck(encoded, reqBuf []byte) float64 {
	k := util.AdaptiveK(len(reqBuf), 3, 8, 5)
	shingles1 := util.CreateShingles(encoded, k)
	shingles2 := util.CreateShingles(reqBuf, k)
	similarity := util.JaccardSimilarity(shingles1, shingles2)
	return similarity
}

func findExactMatch(tcsMocks []*models.Mock, reqBuffs [][]byte) int {
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(reqBuffs) {
			nc := util.NewNoiseChecker(mock.Noise)
			matched := true // Flag to track if all requests match

			for requestIndex, reqBuff := range reqBuffs {
				mockData := mock.Spec.GenericRequests[requestIndex].Message[0].Data

				// If mock data is noisy (obfuscated), skip comparison for this buffer
				if nc != nil && nc.IsNoisy(mockData) {
					continue
				}

				bufStr := string(reqBuff)
				if !util.IsASCII(string(reqBuff)) {
					bufStr = util.EncodeBase64(reqBuff)
				}

				// Compare the encoded data
				if mockData != bufStr {
					matched = false
					break // Exit the loop if any request doesn't match
				}
			}

			if matched {
				return idx
			}
		}
	}
	return -1
}
