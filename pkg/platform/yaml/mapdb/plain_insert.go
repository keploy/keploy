package mapdb

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	yamlLib "gopkg.in/yaml.v3"
)

// tryInsertPlainYAML performs Insert directly on a plain YAML mappings file
// (one in keploy's exact layout with no hand edits) without holding every
// unchanged test's decoded MockEntry structs in memory at once.
//
// Each chunk of existing test entries is decoded, verified byte-for-byte
// against keploy's own encoding (the same plain check decodeInto uses), and
// discarded immediately for tests mapping does not update, keeping only each
// unchanged test's ID and verified byte span in data. The rebuilt file is then
// streamed directly through WriteRecordingFileStreamF, copying unchanged test
// spans verbatim and encoding only the tests mapping updates or adds.
//
// Returns (true, totalTests, err) when data was plain and handled (err is any
// write error), or (false, 0, nil) when data is not plain or cannot be checked
// chunk-by-chunk so Insert falls back to its full decode+encodeOver path.
func (db *MappingDb) tryInsertPlainYAML(ctx context.Context, mapping *models.Mapping, mappingPath, fileName string, data []byte) (bool, int, error) {
	l, ok := findTests(data)
	if !ok {
		return false, 0, nil
	}
	hLen := headLen(data)
	if hLen > l.key {
		return false, 0, nil
	}

	// Decode and plain-check the skeleton (everything outside tests:).
	rest := make([]byte, 0, l.key+len("tests: []\n")+len(data)-l.rest)
	rest = append(rest, data[:l.key]...)
	rest = append(rest, "tests: []\n"...)
	rest = append(rest, data[l.rest:]...)
	var existingSkel models.Mapping
	if err := yamlLib.Unmarshal(rest, &existingSkel); err != nil {
		return false, 0, nil
	}
	checkSkel := existingSkel
	checkSkel.TestCases = []models.MappedTestCase{}
	encSkel, err := yaml.MarshalGeneric(yaml.FormatYAML, &checkSkel)
	if err != nil {
		return false, 0, nil
	}
	wantRest := make([]byte, 0, (l.key-hLen)+len("tests: []\n")+(len(data)-l.rest))
	wantRest = append(wantRest, data[hLen:l.key]...)
	wantRest = append(wantRest, "tests: []\n"...)
	wantRest = append(wantRest, data[l.rest:]...)
	if !bytes.Equal(encSkel, wantRest) {
		return false, 0, nil
	}

	// Index the IDs being written by this Insert.
	incomingIDs := make(map[string]struct{}, len(mapping.TestCases))
	for _, tc := range mapping.TestCases {
		incomingIDs[tc.ID] = struct{}{}
	}

	// Decode and plain-check each test chunk, keeping only byte spans for
	// unchanged tests and decoded MappedTestCase structs for updated ones.
	rawEntries := make(map[string][2]int, len(l.entries))
	var existingReplaced []models.MappedTestCase
	var chunk, indentBuf []byte
	for start, end := 0, 0; start < len(l.entries); start = end {
		end = start + 1
		for end < len(l.entries) && end-start < mappingChunk && l.entries[end][1]-l.entries[start][0] <= chunkBytes {
			end++
		}
		var tests []models.MappedTestCase
		if e := l.entries[start]; end == start+1 && e[1]-e[0] > chunkBytes {
			tc, ok := decodeLargeEntry(data, e, &chunk)
			if !ok {
				return false, 0, nil
			}
			tests = []models.MappedTestCase{tc}
		} else {
			chunk = append(append(chunk[:0], "tests:\n"...), data[l.entries[start][0]:l.entries[end-1][1]]...)
			var part struct {
				Tests []models.MappedTestCase `yaml:"tests"`
			}
			if err := yamlLib.Unmarshal(chunk, &part); err != nil {
				return false, 0, nil
			}
			tests = part.Tests
		}
		if len(tests) != end-start {
			return false, 0, nil
		}
		if !chunkMatchesRaw(tests, data[l.entries[start][0]:l.entries[end-1][1]], &indentBuf) {
			return false, 0, nil
		}
		for k, tc := range tests {
			if _, dup := rawEntries[tc.ID]; dup {
				// Duplicate test IDs in the file take the full path.
				return false, 0, nil
			}
			if _, updating := incomingIDs[tc.ID]; updating {
				existingReplaced = append(existingReplaced, tc)
				continue
			}
			rawEntries[tc.ID] = l.entries[start+k]
		}
	}

	// Build the updated tests through the exact CreateMappingStructure +
	// keepEntries path so every MappedTestCase field is preserved identically.
	updatedMap := make(map[string][]models.MockEntry, len(mapping.TestCases))
	for _, tc := range mapping.TestCases {
		updatedMap[tc.ID] = tc.Mocks
	}
	mini := CreateMappingStructure(mapping.TestSetID, updatedMap, db.logger)
	keepEntries(mini, existingReplaced, mapping.TestCases)
	updatedByID := make(map[string]models.MappedTestCase, len(mini.TestCases))
	for _, tc := range mini.TestCases {
		updatedByID[tc.ID] = tc
	}

	// Build the output skeleton with the exact replace-or-keep set sections.
	outSkel := &models.Mapping{
		Version:   string(models.V1Beta1),
		Kind:      models.MappingKind,
		TestSetID: mapping.TestSetID,
		TestCases: []models.MappedTestCase{},
	}
	applySetSections(outSkel, &existingSkel, mapping)
	skelBytes, err := yaml.MarshalGeneric(yaml.FormatYAML, outSkel)
	if err != nil {
		return false, 0, nil
	}
	const emptyTests = "\ntests: []\n"
	at := bytes.Index(skelBytes, []byte(emptyTests))
	if at < 0 {
		return false, 0, nil
	}

	allIDs := make([]string, 0, len(rawEntries)+len(updatedByID))
	for id := range rawEntries {
		allIDs = append(allIDs, id)
	}
	for id := range updatedByID {
		allIDs = append(allIDs, id)
	}
	sort.Strings(allIDs)

	head := commentHead(data)
	if len(head) == 0 {
		head = []byte(utils.GetVersionAsComment())
	}

	db.live.forget()
	err = yaml.WriteRecordingFileStreamF(ctx, db.logger, db.path, mappingPath, fileName, yaml.FormatYAML, func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 64<<10)
		if _, err := bw.Write(head); err != nil {
			return err
		}
		if len(allIDs) == 0 {
			if _, err := bw.Write(skelBytes); err != nil {
				return err
			}
			return bw.Flush()
		}
		if _, err := bw.Write(skelBytes[:at]); err != nil {
			return err
		}
		if _, err := bw.WriteString("\ntests:\n"); err != nil {
			return err
		}
		var encBuf []byte
		writeIndented := func(p []byte) error {
			encBuf = indented(encBuf[:0], p, testsIndent)
			_, err := bw.Write(encBuf)
			return err
		}
		for _, id := range allIDs {
			if tc, ok := updatedByID[id]; ok {
				if len(tc.Mocks) > chunkMocks {
					var werr error
					_, err := encodeLargeEntry(tc, func(p []byte) bool {
						if err := writeIndented(p); err != nil {
							werr = err
							return false
						}
						return true
					})
					if err != nil {
						return err
					}
					if werr != nil {
						return werr
					}
					continue
				}
				part, err := yaml.MarshalGeneric(yaml.FormatYAML, []models.MappedTestCase{tc})
				if err != nil {
					return err
				}
				if err := writeIndented(part); err != nil {
					return err
				}
				continue
			}
			span := rawEntries[id]
			if _, err := bw.Write(data[span[0]:span[1]]); err != nil {
				return err
			}
		}
		if _, err := bw.Write(skelBytes[at+len(emptyTests):]); err != nil {
			return err
		}
		return bw.Flush()
	})
	return true, len(allIDs), err
}

// chunkMatchesRaw reports whether tests encodes byte-for-byte to raw under
// encodeMappingYAML's test-list indentation.
func chunkMatchesRaw(tests []models.MappedTestCase, raw []byte, indentBuf *[]byte) bool {
	pos, same := 0, true
	check := func(p []byte) bool {
		*indentBuf = indented((*indentBuf)[:0], p, testsIndent)
		if len(*indentBuf) > len(raw)-pos || !bytes.Equal(raw[pos:pos+len(*indentBuf)], *indentBuf) {
			same = false
			return false
		}
		pos += len(*indentBuf)
		return true
	}
	for start, end := 0, 0; start < len(tests); start = end {
		end = chunkEnd(tests, start)
		if end == start+1 && len(tests[start].Mocks) > chunkMocks {
			more, err := encodeLargeEntry(tests[start], check)
			if err != nil || !more {
				return false
			}
			continue
		}
		part, err := yaml.MarshalGeneric(yaml.FormatYAML, tests[start:end])
		if err != nil || !check(part) {
			return false
		}
	}
	return same && pos == len(raw)
}

// applySetSections applies Insert's replace-or-keep rules for every set-level
// section on dst from existing and incoming.
func applySetSections(dst, existing, incoming *models.Mapping) {
	switch {
	case len(incoming.Startup) > 0:
		dst.Startup = incoming.Startup
	case len(existing.Startup) > 0:
		dst.Startup = existing.Startup
	}
	switch {
	case len(incoming.Worker) > 0:
		dst.Worker = incoming.Worker
	case len(existing.Worker) > 0:
		dst.Worker = existing.Worker
	}
	switch {
	case len(incoming.Groups) > 0 || len(incoming.Shared) > 0:
		dst.Groups, dst.Shared = incoming.Groups, incoming.Shared
	default:
		dst.Groups, dst.Shared = existing.Groups, existing.Shared
	}
	dst.Source = existing.Source
	if incoming.Source != "" {
		dst.Source = incoming.Source
	}
	dst.RecordedWith = existing.RecordedWith
	if len(incoming.RecordedWith) > 0 {
		dst.RecordedWith = incoming.RecordedWith
	}
	if len(incoming.Suites) == 0 {
		dst.Suites = existing.Suites
	} else {
		dst.Suites = incoming.Suites
	}
	if len(incoming.Boots) == 0 {
		dst.Boots = existing.Boots
	} else {
		dst.Boots = incoming.Boots
	}
	if len(incoming.Runner) == 0 {
		dst.Runner = existing.Runner
	} else {
		dst.Runner = incoming.Runner
	}
}
