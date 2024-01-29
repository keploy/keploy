package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/fatih/color"
	"github.com/olekukonko/tablewriter"
	"github.com/yudai/gojsondiff"
	"github.com/yudai/gojsondiff/formatter"
)

// Chars PER expected/actual string. Can be changed no problem
const MAX_LINE_LENGTH = 50

type DiffsPrinter struct {
	testCase                   string
	statusExp                  string
	statusAct                  string
	headerExp                  map[string]string
	headerAct                  map[string]string
	bodyExp                    string
	bodyAct                    string
	bodyNoise                  map[string][]string
	headNoise                  map[string][]string
	hasSameDifferentOrderMocks bool
	text                       string
}

func NewDiffsPrinter(testCase string) DiffsPrinter {
	return DiffsPrinter{testCase, "", "", map[string]string{}, map[string]string{}, "", "", map[string][]string{}, map[string][]string{}, false, ""}
}

func (d *DiffsPrinter) PushStatusDiff(exp, act string) {
	d.statusExp, d.statusAct = exp, act
}

func (d *DiffsPrinter) PushFooterDiff(key string) {
	d.hasSameDifferentOrderMocks = true
	d.text = key
}

func (d *DiffsPrinter) PushHeaderDiff(exp, act, key string, noise map[string][]string) {
	d.headerExp[key], d.headerAct[key], d.headNoise = exp, act, noise
}

func (d *DiffsPrinter) PushBodyDiff(exp, act string, noise map[string][]string) {
	d.bodyExp, d.bodyAct, d.bodyNoise = exp, act, noise
}

// Will display and colorize diffs side-by-side
func (d *DiffsPrinter) Render() error {
	diffs := []string{}

	if d.statusExp != d.statusAct {
		diffs = append(diffs, sprintDiff(d.statusExp, d.statusAct, "status"))
	}

	diffs = append(diffs, sprintDiffHeader(d.headerExp, d.headerAct))

	if len(d.bodyExp) != 0 || len(d.bodyAct) != 0 {
		bE, bA := []byte(d.bodyExp), []byte(d.bodyAct)
		if json.Valid(bE) && json.Valid(bA) {
			difference, err := sprintJSONDiff(bE, bA, "body", d.bodyNoise)
			if err != nil {
				difference = sprintDiff(d.bodyExp, d.bodyAct, "body")
			}
			diffs = append(diffs, difference)
		} else {
			diffs = append(diffs, sprintDiff(d.bodyExp, d.bodyAct, "body"))
		}

	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{fmt.Sprintf("Diffs %v", d.testCase)})
	table.SetHeaderColor(tablewriter.Colors{tablewriter.FgHiRedColor})
	table.SetAlignment(tablewriter.ALIGN_CENTER)

	for _, e := range diffs {
		table.Append([]string{e})
	}
	if d.hasSameDifferentOrderMocks {
		table.SetHeader([]string{d.text})
		table.SetAlignment(tablewriter.ALIGN_CENTER)
		paint := color.New(color.FgYellow).SprintFunc()
		postPaint := paint(d.text)
		table.Append([]string{postPaint})

	}
	table.Render()
	return nil
}

/*
 * Returns a nice diff table where the left is the expect and the right
 * is the actual. each entry in expect and actual will contain the key
 * and the corresponding value.
 */
func sprintDiffHeader(expect, actual map[string]string) string {

	expectAll := ""
	actualAll := ""
	for key, expValue := range expect {
		actValue := key + ": " + actual[key]
		expValue = key + ": " + expValue
		// Offset will be where the string start to unmatch
		offset, _ := diffIndex(expValue, actValue)

		// Color of the unmatch, can be changed
		cE, cA := color.FgHiRed, color.FgHiGreen

		expectAll += breakWithColor(expValue, &cE, offset)
		actualAll += breakWithColor(actValue, &cA, offset)
	}
	if len(expect) > MAX_LINE_LENGTH || len(actual) > MAX_LINE_LENGTH {
		return expectActualTable(expectAll, actualAll, "header", false) // Don't centerize
	}
	return expectActualTable(expectAll, actualAll, "header", true)
}

/*
 * Returns a nice diff table where the left is the expect and the right
 * is the actual. For JSON-based diffs use SprintJSONDiff
 * field: body, status...
 */
func sprintDiff(expect, actual, field string) string {

	// Offset will be where the string start to unmatch
	offset, _ := diffIndex(expect, actual)

	// Color of the unmatch, can be changed
	cE, cA := color.FgHiRed, color.FgHiGreen

	exp := breakWithColor(expect, &cE, offset)
	act := breakWithColor(actual, &cA, offset)
	if len(expect) > MAX_LINE_LENGTH || len(actual) > MAX_LINE_LENGTH {
		return expectActualTable(exp, act, field, false) // Don't centerize
	}
	return expectActualTable(exp, act, field, true)
}

/* This will return the json diffs in a beautifull way. It will in fact
 * create a colorized table-based expect-response string and return it.
 * on the left-side there'll be the expect and on the right the actual
 * response. Its important to mention the inputs must to be a json. If
 * the body isnt in the rest-api formats (what means it is not json-based)
 * its better to use a generic diff output as the SprintDiff.
 */
func sprintJSONDiff(json1 []byte, json2 []byte, field string, noise map[string][]string) (string, error) {
	diffString, err := calculateJSONDiffs(json1, json2)
	if err != nil {
		return "", err
	}
	expect, actual := separateAndColorize(diffString, noise)
	result := expectActualTable(expect, actual, field, false)
	return result, nil
}

// Find the diff between two strings returning index where
// the difference begin
func diffIndex(s1, s2 string) (int, bool) {
	diff := false
	i := -1

	// Check if one string is smaller than another, if so theres a diff
	if len(s1) < len(s2) {
		i = len(s1)
		diff = true
	} else if len(s2) < len(s1) {
		diff = true
		i = len(s2)
	}

	// Check for unmatched characters
	for i := 0; i < len(s1) && i < len(s2); i++ {
		if s1[i] != s2[i] {
			return i, true
		}
	}

	return i, diff
}

/* Will perform the calculation of the diffs, returning a string that
 * containes the lines that does not match represented by either a
 * minus or add symbol followed by the respective line.
 */
func calculateJSONDiffs(json1 []byte, json2 []byte) (string, error) {
	var diff = gojsondiff.New()
	dObj, err := diff.Compare(json1, json2)
	if err != nil {
		return "", err
	}

	var jsonObject map[string]interface{}
	err = json.Unmarshal([]byte(json1), &jsonObject)
	if err != nil {
		return "", err
	}

	diffString, _ := formatter.NewAsciiFormatter(jsonObject, formatter.AsciiFormatterConfig{
		ShowArrayIndex: true,
		Coloring:       false, // We will color our way
	}).Format(dObj)

	return diffString, nil
}

// Will receive a string that has the differences represented
// by a plus or a minus sign and separate it. Just works with json
func separateAndColorize(diffStr string, noise map[string][]string) (string, string) {
	expect, actual := "", ""

	diffLines := strings.Split(diffStr, "\n")

	for i, line := range diffLines {
		if len(line) > 0 {
			noised := false

			for e := range noise {
				// If contains noise remove diff flag
				if strings.Contains(line, e) {

					if line[0] == '-' {
						line = " " + line[1:]
						expect += breakWithColor(line, nil, 0)
					} else if line[0] == '+' {
						line = " " + line[1:]
						actual += breakWithColor(line, nil, 0)
					}
					noised = true
				}
			}

			if noised {
				continue
			}

			if line[0] == '-' {
				c := color.FgRed

				// Workaround to get the exact index where the diff begins
				if diffLines[i+1][0] == '+' {

					/* As we want to get the exact difference where the line's
					 * diff begin we must to, first, get the expect (this) and
					 * the actual (next) line. Then we must to espace the first
					 * char that is an "+" or "-" symbol so we end up having
					 * just the contents of the line we want to compare */
					offset, _ := diffIndex(line[1:], diffLines[i+1][1:])
					expect += breakWithColor(line, &c, offset+1)
				} else {
					// In the case where there isn't in fact an actual
					// version to compare, it was just expect to have this
					expect += breakWithColor(line, &c, 0)
				}
			} else if line[0] == '+' {
				c := color.FgGreen

				// Here we do the same thing as above, just inverted
				if diffLines[i-1][0] == '-' {
					offset, _ := diffIndex(line[1:], diffLines[i-1][1:])
					actual += breakWithColor(line, &c, offset+1)
				} else {
					actual += breakWithColor(line, &c, 0)
				}
			} else {
				expect += breakWithColor(line, nil, 0)
				actual += breakWithColor(line, nil, 0)
			}
		}
	}

	return expect, actual
}

// Will colorize the strubg and do the job of break it if it pass MAX_LINE_LENGTH,
// always respecting the reset of ascii colors before the break line to dont
func breakWithColor(input string, c *color.Attribute, offset int) string {
	var output []string
	var paint func(a ...interface{}) string
	colorize := false

	if c != nil {
		colorize = true
		paint = color.New(*c).SprintFunc()
	}

	for i := 0; i < len(input); i += MAX_LINE_LENGTH {
		end := i + MAX_LINE_LENGTH

		if end > len(input) {
			end = len(input)
		}

		// This conditions joins if we are at line where the offset begins
		if colorize && i+MAX_LINE_LENGTH > offset {
			paintedStart := i
			if paintedStart < offset {
				paintedStart = offset
			}

			// Will basically concatenated the non-painted string with the
			// painted
			prePaint := input[i:paintedStart]           // Start at i ends at offset
			postPaint := paint(input[paintedStart:end]) // Starts at offset (diff begins), goes til maxLength
			substr := prePaint + postPaint + "\n"       // Concatenate
			output = append(output, substr)
		} else {
			substr := input[i:end] + "\n"
			output = append(output, substr)
		}
	}
	return strings.Join(output, "")
}

// Will return a string in a two columns table where the left
// side is the expected string and the right is the actual
// field: body, header, status...
func expectActualTable(exp string, act string, field string, centerize bool) string {
	buf := &bytes.Buffer{}
	table := tablewriter.NewWriter(buf)

	if centerize {
		table.SetAlignment(tablewriter.ALIGN_CENTER)
	}

	table.SetHeader([]string{fmt.Sprintf("Expect %v", field), fmt.Sprintf("Actual %v", field)})
	table.SetAutoWrapText(false)
	table.SetBorder(false)
	table.SetColMinWidth(0, MAX_LINE_LENGTH)
	table.SetColMinWidth(1, MAX_LINE_LENGTH)
	table.Append([]string{exp, act})
	table.Render()
	return buf.String()
}
