//go:build linux

package redis

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/service/vector"
	"go.uber.org/zap"
)

var (
	vectorIntegration     *vector.IntegrationService
	vectorIntegrationOnce sync.Once
	logger                *zap.Logger
	
	// Context tracking for better matching
	requestContext        = make(map[string][]byte) // Map of connection IDs to context
	requestContextMutex   sync.RWMutex
	maxContextSize        = 10                      // Maximum number of requests to keep in context
)

// initVectorIntegration initializes the vector database integration service
func initVectorIntegration(ctx context.Context, log *zap.Logger) {
	vectorIntegrationOnce.Do(func() {
		logger = log
		if logger == nil {
			// Create a default logger if none is provided
			var err error
			logger, err = zap.NewProduction()
			if err != nil {
				// If we can't create a logger, fall back to fuzzy matching
				return
			}
		}
		
		vectorIntegration = vector.NewIntegrationService(logger)
		err := vectorIntegration.Initialize(ctx)
		if err != nil {
			logger.Error("Failed to initialize vector database integration", zap.Error(err))
			vectorIntegration = nil
		} else if vectorIntegration.Enabled {
			logger.Info("Vector database integration initialized successfully")
		}
	})
}

// fuzzyMatch performs a fuzzy matching algorithm to find the best matching mock for the given request.
// It takes a context, a request buffer, and a mock database as input parameters.
// The function iterates over the mocks in the database and applies the fuzzy matching algorithm to find the best match.
// If a match is found, it returns the corresponding response mock and a boolean value indicating success.
// If no match is found, it returns false and a nil response.
// If an error occurs during the matching process, it returns an error.
func fuzzyMatch(ctx context.Context, reqBuff [][]byte, mockDb integrations.MockMemDb) (bool, []models.Payload, error) {
	// Extract connection ID from context if available
	connID := extractConnectionID(ctx)
	
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:
			mocks, err := mockDb.GetUnFilteredMocks()
			if err != nil {
				return false, nil, fmt.Errorf("error while getting unfiltered mocks %v", err)
			}

			var filteredMocks []*models.Mock
			var unfilteredMocks []*models.Mock

			for _, mock := range mocks {
				if mock.Kind != "Redis" {
					continue
				}
				if mock.TestModeInfo.IsFiltered {
					filteredMocks = append(filteredMocks, mock)
				} else {
					unfilteredMocks = append(unfilteredMocks, mock)
				}
			}

			// First try exact matching on filtered mocks
			index := findExactMatch(filteredMocks, reqBuff)

			// If no exact match, try vector similarity matching if enabled
			if index == -1 && len(reqBuff) > 0 {
				// Initialize vector integration if not already done
				initVectorIntegration(ctx, logger)
				
				// If vector integration is enabled, try to find a match using vector similarity
				if vectorIntegration != nil && vectorIntegration.Enabled {
					// Enhance request with context for better matching
					contextEnhancedReqBuff := enhanceWithContext(reqBuff[0], connID)
					
					// First try context-enhanced matching
					idx, found := findMockByVectorSimilarityWithContext(ctx, reqBuff[0], contextEnhancedReqBuff, filteredMocks, "Redis")
					if found {
						index = idx
						if logger != nil {
							logger.Debug("Found vector similarity match with context in filtered mocks", 
								zap.Int("index", index))
						}
					} else {
						// Fall back to regular vector matching if context matching fails
						idx, found := vectorIntegration.FindMockByVectorSimilarity(ctx, reqBuff[0], filteredMocks, "Redis")
						if found {
							index = idx
							if logger != nil {
								logger.Debug("Found vector similarity match in filtered mocks", 
									zap.Int("index", index))
							}
						}
					}
				}
			}

			// If still no match, try binary matching on filtered mocks
			if index == -1 {
				index = findBinaryMatch(filteredMocks, reqBuff, 0.9)
			}

			if index != -1 {
				responseMock := make([]models.Payload, len(filteredMocks[index].Spec.RedisResponses))
				copy(responseMock, filteredMocks[index].Spec.RedisResponses)
				originalFilteredMock := *filteredMocks[index]
				filteredMocks[index].TestModeInfo.IsFiltered = false
				filteredMocks[index].TestModeInfo.SortOrder = math.MaxInt64
				isUpdated := mockDb.UpdateUnFilteredMock(&originalFilteredMock, filteredMocks[index])
				if !isUpdated {
					continue
				}
				
				// Update context with this request for future matching
				updateRequestContext(connID, reqBuff[0])
				
				return true, responseMock, nil
			}

			// Try exact matching on unfiltered mocks
			index = findExactMatch(unfilteredMocks, reqBuff)

			// If no exact match, try vector similarity matching if enabled
			if index == -1 && len(reqBuff) > 0 {
				if vectorIntegration != nil && vectorIntegration.Enabled {
					// Enhance request with context for better matching
					contextEnhancedReqBuff := enhanceWithContext(reqBuff[0], connID)
					
					// First try context-enhanced matching
					idx, found := findMockByVectorSimilarityWithContext(ctx, reqBuff[0], contextEnhancedReqBuff, unfilteredMocks, "Redis")
					if found {
						index = idx
						if logger != nil {
							logger.Debug("Found vector similarity match with context in unfiltered mocks", 
								zap.Int("index", index))
						}
					} else {
						// Fall back to regular vector matching if context matching fails
						idx, found := vectorIntegration.FindMockByVectorSimilarity(ctx, reqBuff[0], unfilteredMocks, "Redis")
						if found {
							index = idx
							if logger != nil {
								logger.Debug("Found vector similarity match in unfiltered mocks", 
									zap.Int("index", index))
							}
						}
					}
				}
			}

			if index != -1 {
				responseMock := make([]models.Payload, len(unfilteredMocks[index].Spec.RedisResponses))
				copy(responseMock, unfilteredMocks[index].Spec.RedisResponses)
				
				// Update context with this request for future matching
				updateRequestContext(connID, reqBuff[0])
				
				return true, responseMock, nil
			}

			// Try binary matching on all mocks as a last resort
			totalMocks := append(filteredMocks, unfilteredMocks...)
			index = findBinaryMatch(totalMocks, reqBuff, 0.4)

			// If still no match and vector similarity is enabled, try vector matching with a lower threshold
			if index == -1 && len(reqBuff) > 0 && vectorIntegration != nil && vectorIntegration.Enabled && vectorIntegration.ShouldFallbackToFuzzy() {
				// Enhance request with context for better matching
				contextEnhancedReqBuff := enhanceWithContext(reqBuff[0], connID)
				
				// First try context-enhanced matching with lower threshold
				idx, found := findMockByVectorSimilarityWithContext(ctx, reqBuff[0], contextEnhancedReqBuff, totalMocks, "Redis")
				if found {
					index = idx
					if logger != nil {
						logger.Debug("Found vector similarity match with context in total mocks", 
							zap.Int("index", index))
					}
				} else {
					// Fall back to regular vector matching if context matching fails
					idx, found := vectorIntegration.FindMockByVectorSimilarity(ctx, reqBuff[0], totalMocks, "Redis")
					if found {
						index = idx
						if logger != nil {
							logger.Debug("Found vector similarity match in total mocks", 
								zap.Int("index", index))
						}
					}
				}
			}

			if index != -1 {
				responseMock := make([]models.Payload, len(totalMocks[index].Spec.RedisResponses))
				copy(responseMock, totalMocks[index].Spec.RedisResponses)
				originalFilteredMock := *totalMocks[index]
				if totalMocks[index].TestModeInfo.IsFiltered {
					totalMocks[index].TestModeInfo.IsFiltered = false
					totalMocks[index].TestModeInfo.SortOrder = math.MaxInt64
					isUpdated := mockDb.UpdateUnFilteredMock(&originalFilteredMock, totalMocks[index])
					if !isUpdated {
						continue
					}
				}
				
				// Update context with this request for future matching
				updateRequestContext(connID, reqBuff[0])
				
				return true, responseMock, nil
			}

			// Index all mocks in the vector database if vector integration is enabled
			if vectorIntegration != nil && vectorIntegration.Enabled {
				redisMocks := make([]*models.Mock, 0)
				for _, mock := range mocks {
					if mock.Kind == "Redis" {
						redisMocks = append(redisMocks, mock)
					}
				}
				// This is done asynchronously to avoid blocking the response
				go func(mocks []*models.Mock) {
					err := vectorIntegration.IndexMocks(context.Background(), mocks)
					if err != nil && logger != nil {
						logger.Error("Failed to index mocks in vector database", zap.Error(err))
					}
				}(redisMocks)
			}
			
			// Update context even if no match was found
			if len(reqBuff) > 0 {
				updateRequestContext(connID, reqBuff[0])
			}

			return false, nil, nil
		}
	}
}

// findMockByVectorSimilarityWithContext finds a mock by vector similarity with context enhancement
func findMockByVectorSimilarityWithContext(ctx context.Context, reqBuff, contextEnhancedReqBuff []byte, mocks []*models.Mock, kind string) (int, bool) {
	if vectorIntegration == nil || !vectorIntegration.Enabled {
		return -1, false
	}
	
	// First try using the context-enhanced request buffer
	if len(contextEnhancedReqBuff) > 0 && !bytes.Equal(reqBuff, contextEnhancedReqBuff) {
		idx, found := vectorIntegration.FindMockByVectorSimilarity(ctx, contextEnhancedReqBuff, mocks, kind)
		if found {
			return idx, true
		}
	}
	
	// Fall back to regular request buffer
	return vectorIntegration.FindMockByVectorSimilarity(ctx, reqBuff, mocks, kind)
}

// extractConnectionID gets the connection ID from the context
func extractConnectionID(ctx context.Context) string {
	// Get connection ID from context if available
	// This is a placeholder - implement based on your context structure
	connIDValue := ctx.Value("connection_id")
	if connIDValue != nil {
		if connID, ok := connIDValue.(string); ok {
			return connID
		}
	}
	
	// Default to a fixed ID if not available
	return "default_connection"
}

// updateRequestContext adds a request to the context for a connection
func updateRequestContext(connID string, reqBuff []byte) {
	requestContextMutex.Lock()
	defer requestContextMutex.Unlock()
	
	// Initialize context for this connection if it doesn't exist
	if _, exists := requestContext[connID]; !exists {
		requestContext[connID] = make([]byte, 0)
	}
	
	// Combine existing context with new request
	combinedContext := append(requestContext[connID], reqBuff...)
	
	// Limit context size to prevent unbounded growth
	if len(combinedContext) > maxContextSize*1024 {
		// Keep only the most recent portion of the context
		startIdx := len(combinedContext) - maxContextSize*1024
		if startIdx < 0 {
			startIdx = 0
		}
		combinedContext = combinedContext[startIdx:]
	}
	
	requestContext[connID] = combinedContext
	
	// Set expiration for this context
	go func(id string) {
		time.Sleep(30 * time.Minute)
		requestContextMutex.Lock()
		defer requestContextMutex.Unlock()
		delete(requestContext, id)
	}(connID)
}

// enhanceWithContext combines the current request with context from previous requests
func enhanceWithContext(reqBuff []byte, connID string) []byte {
	requestContextMutex.RLock()
	defer requestContextMutex.RUnlock()
	
	existingContext, exists := requestContext[connID]
	if !exists || len(existingContext) == 0 {
		return reqBuff
	}
	
	// Combine context with current request
	// Format: [previous requests context] [current request]
	enhancedBuff := make([]byte, len(existingContext)+len(reqBuff))
	copy(enhancedBuff, existingContext)
	copy(enhancedBuff[len(existingContext):], reqBuff)
	
	return enhancedBuff
}

// TODO: need to generalize this function for different types of integrations.
func findBinaryMatch(tcsMocks []*models.Mock, reqBuffs [][]byte, mxSim float64) int {
	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.RedisRequests) == len(reqBuffs) {
			for requestIndex, reqBuff := range reqBuffs {
				mockReq, err := util.DecodeBase64(mock.Spec.RedisRequests[requestIndex].Message[0].Data)
				if err != nil {
					mockReq = []byte(mock.Spec.RedisRequests[requestIndex].Message[0].Data)
				}

				similarity := fuzzyCheck(mockReq, reqBuff)
				if mxSim < similarity {
					mxSim = similarity
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
		if len(mock.Spec.RedisRequests) == len(reqBuffs) {
			matched := true // Flag to track if all requests match

			for requestIndex, reqBuff := range reqBuffs {

				bufStr := string(reqBuff)

				// Compare the encoded data
				if mock.Spec.RedisRequests[requestIndex].Message[0].Data != bufStr {
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
