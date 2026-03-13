package tablewriter

import (
	"io"

	"github.com/fatih/color"
	otw "github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
)

const (
	ALIGN_LEFT = iota
	ALIGN_CENTER
	ALIGN_RIGHT
)

const (
	FgHiRedColor = iota + 1
	FgHiGreenColor
)

type Colors []int

type Table struct {
	table        *otw.Table
	header       []string
	headerColors []Colors
}

func NewWriter(w io.Writer) *Table {
	return &Table{table: otw.NewWriter(w)}
}

func (t *Table) SetAutoWrapText(enabled bool) {
	wrap := tw.WrapNone
	if enabled {
		wrap = tw.WrapNormal
	}
	t.table.Options(
		otw.WithHeaderAutoWrap(wrap),
		otw.WithRowAutoWrap(wrap),
		otw.WithFooterAutoWrap(wrap),
	)
}

func (t *Table) SetReflowDuringAutoWrap(bool) {}

func (t *Table) SetHeader(headers []string) {
	t.header = append([]string(nil), headers...)
	t.applyHeader()
}

func (t *Table) SetHeaderColor(colors ...Colors) {
	t.headerColors = append([]Colors(nil), colors...)
	if len(t.header) > 0 {
		t.applyHeader()
	}
}

func (t *Table) SetAlignment(alignment int) {
	align := mapAlignment(alignment)
	t.table.Options(
		otw.WithRowAlignment(align),
		otw.WithFooterAlignmentConfig(tw.CellAlignment{Global: align}),
	)
}

func (t *Table) SetHeaderAlignment(alignment int) {
	t.table.Options(otw.WithHeaderAlignment(mapAlignment(alignment)))
}

func (t *Table) SetBorder(enabled bool) {
	if enabled {
		return
	}
	t.table.Options(otw.WithRendition(tw.Rendition{
		Borders: tw.BorderNone,
		Settings: tw.Settings{
			Lines:      tw.LinesNone,
			Separators: tw.SeparatorsNone,
		},
	}))
}

func (t *Table) SetColMinWidth(int, int) {}

func (t *Table) SetColWidth(int) {}

func (t *Table) SetTablePadding(string) {}

func (t *Table) SetAutoMergeCells(bool) {}

func (t *Table) SetRowLine(bool) {}

func (t *Table) SetCenterSeparator(string) {}

func (t *Table) SetColumnSeparator(string) {}

func (t *Table) SetRowSeparator(string) {}

func (t *Table) Append(row []string) {
	_ = t.table.Append(row)
}

func (t *Table) Render() {
	_ = t.table.Render()
}

func (t *Table) applyHeader() {
	headers := append([]string(nil), t.header...)
	for i, cfg := range t.headerColors {
		if i >= len(headers) {
			break
		}
		headers[i] = colorize(headers[i], cfg)
	}
	t.table.Header(headers)
}

func mapAlignment(alignment int) tw.Align {
	switch alignment {
	case ALIGN_CENTER:
		return tw.AlignCenter
	case ALIGN_RIGHT:
		return tw.AlignRight
	default:
		return tw.AlignLeft
	}
}

func colorize(s string, colors Colors) string {
	attrs := make([]color.Attribute, 0, len(colors))
	for _, c := range colors {
		switch c {
		case FgHiRedColor:
			attrs = append(attrs, color.FgHiRed)
		case FgHiGreenColor:
			attrs = append(attrs, color.FgHiGreen)
		}
	}
	if len(attrs) == 0 {
		return s
	}
	return color.New(attrs...).Sprint(s)
}
