package utils

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	jsonDiff "github.com/keploy/jsonDiff"
	"github.com/olekukonko/tablewriter"
	"go.uber.org/zap"
)

type DiffService struct {
	logger *zap.Logger
}

func NewDiffService(logger *zap.Logger) *DiffService {
	return &DiffService{logger: logger}
}

func (d *DiffService) Compare(json1, json2 string) (string, error) {
	diff, err := jsonDiff.CompareJSON([]byte(json1), []byte(json2), nil, false)
	if err != nil {
		d.logger.Error("Error in JSON diff", zap.Error(err))
		return "", err
	}
	return expectActualTable(diff.Actual, diff.Expected, "", false), nil
}

func expectActualTable(exp string, act string, field string, centerize bool) string {
	buf := &bytes.Buffer{}
	table := tablewriter.NewWriter(buf)

	if centerize {
		table.SetAlignment(tablewriter.ALIGN_CENTER)
	} else {
		table.SetAlignment(tablewriter.ALIGN_LEFT)
	}

	exp = wrapTextWithAnsi(exp)
	act = wrapTextWithAnsi(act)
	table.SetHeader([]string{fmt.Sprintf("Expect %v", field), fmt.Sprintf("Actual %v", field)})
	table.SetAutoWrapText(false)
	table.SetBorder(false)
	table.SetColMinWidth(0, 50)
	table.SetColMinWidth(1, 50)
	table.Append([]string{exp, act})
	table.Render()
	return buf.String()
}

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
var ansiResetCode = "\x1b[0m"

func wrapTextWithAnsi(input string) string {
	scanner := bufio.NewScanner(strings.NewReader(input))
	var wrappedBuilder strings.Builder
	currentAnsiCode := ""
	lastAnsiCode := ""

	for scanner.Scan() {
		line := scanner.Text()

		if currentAnsiCode != "" {
			wrappedBuilder.WriteString(currentAnsiCode)
		}

		startAnsiCodes := ansiRegex.FindAllString(line, -1)
		if len(startAnsiCodes) > 0 {
			lastAnsiCode = startAnsiCodes[len(startAnsiCodes)-1]
		}

		wrappedBuilder.WriteString(line)

		if (currentAnsiCode != "" && !strings.HasSuffix(line, ansiResetCode)) || len(startAnsiCodes) > 0 {
			wrappedBuilder.WriteString(ansiResetCode)
			currentAnsiCode = lastAnsiCode
		} else {
			currentAnsiCode = ""
		}

		wrappedBuilder.WriteString("\n")
	}

	return wrappedBuilder.String()
}
