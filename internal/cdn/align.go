package cdn

import (
	"strings"

	"github.com/kamakiri-labs/kamakiri/internal/i18n"
)

// alignGap is the blank a column keeps after its widest cell. It is two
// columns because the watch tables in this package still render through
// text/tabwriter with a two-column gutter, and the two table styles have to
// agree on their spacing.
const alignGap = 2

// alignRows lays a table of cells out into printable lines, measuring a cell in
// terminal columns rather than in runes.
//
// text/tabwriter cannot do this job here: it counts runes and offers no width
// hook, so a Japanese cell that is not last in its row reads as half the columns
// a terminal gives it and every column after it is skewed. Pre-padding the cells
// does not rescue it either, since it then measures the padded cells and pads
// them a second time to its own rune-counted width.
//
// The geometry it does implement is worth keeping, because the status table
// mixes row shapes: a column is sized over a block of consecutive rows that all
// carry a cell in it followed by another, and the first row without one ends the
// block, so a two-cell row between two three-cell rows leaves them measuring
// their second column separately. The last cell of a row is never padded, so no
// line ends in whitespace it does not need.
func alignRows(rows [][]string) []string {
	widths := make([][]int, len(rows))
	for i, row := range rows {
		widths[i] = make([]int, len(row))
	}

	for column := 0; ; column++ {
		measured := false
		for start := 0; start < len(rows); {
			if len(rows[start]) <= column+1 {
				start++
				continue
			}
			measured = true
			end, widest := start, 0
			for end < len(rows) && len(rows[end]) > column+1 {
				widest = max(widest, i18n.Width(rows[end][column]))
				end++
			}
			width := widest + alignGap
			for i := start; i < end; i++ {
				widths[i][column] = width
			}
			start = end
		}
		if !measured {
			break
		}
	}

	lines := make([]string, 0, len(rows))
	for i, row := range rows {
		var line strings.Builder
		for c, cell := range row {
			if c == len(row)-1 {
				line.WriteString(cell)
				continue
			}
			line.WriteString(i18n.Pad(cell, widths[i][c]))
		}
		lines = append(lines, line.String())
	}
	return lines
}
