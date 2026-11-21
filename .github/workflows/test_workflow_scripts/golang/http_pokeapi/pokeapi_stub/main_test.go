package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocationList(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/location-area", nil)

	newHandler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response locationList
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Results, len(locations))
	for i, location := range locations {
		require.Equal(t, location, response.Results[i].Name)
	}
}

func TestLocationAreaIsLargeAndParseable(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/location-area/eterna-city-area", nil)

	newHandler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	body, err := io.ReadAll(recorder.Result().Body)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(body), 8*1024)

	var response locationArea
	require.NoError(t, json.Unmarshal(body, &response))
	require.Equal(t, "eterna-city-area", response.Name)
	require.Len(t, response.PokemonEncounters, 3)
	require.Equal(t, "eevee", response.PokemonEncounters[1].Pokemon.Name)
}

func TestUnknownLocation(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/location-area/missing", nil)

	newHandler().ServeHTTP(recorder, request)

	require.Equal(t, http.StatusNotFound, recorder.Code)
}
