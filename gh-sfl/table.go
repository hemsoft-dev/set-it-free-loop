package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/term"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

type tableCell struct {
	text    string
	styled  string
	styleFn func(string) string
}

func (c tableCell) withText(text string) tableCell {
	styled := text
	if c.styleFn != nil {
		styled = c.styleFn(text)
	}
	return tableCell{text: text, styled: styled, styleFn: c.styleFn}
}

type tableStyler struct {
	output       *termenv.Output
	colorEnabled bool
}

func newTableStyler(w io.Writer, colorEnabled bool) tableStyler {
	profile := termenv.Ascii
	if colorEnabled {
		profile = termenv.ANSI
	}
	output := termenv.NewOutput(w, termenv.WithProfile(profile))
	return tableStyler{output: output, colorEnabled: colorEnabled}
}

func (s tableStyler) colored(text string, color termenv.ANSIColor) tableCell {
	fn := func(t string) string {
		return s.output.String(t).Foreground(color).String()
	}
	return tableCell{text: text, styled: fn(text), styleFn: fn}
}

func (s tableStyler) dim(text string) tableCell {
	fn := func(t string) string {
		return s.output.String(t).Faint().String()
	}
	return tableCell{text: text, styled: fn(text), styleFn: fn}
}

func (s tableStyler) plain(text string) tableCell {
	return tableCell{text: text, styled: text}
}

func (s tableStyler) linkCell(text, url string, color termenv.ANSIColor) tableCell {
	fn := func(t string) string {
		styled := s.output.String(t).Foreground(color).String()
		if url != "" && s.colorEnabled {
			styled = fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", url, styled)
		}
		return styled
	}
	return tableCell{text: text, styled: fn(text), styleFn: fn}
}

func writeRow(w io.Writer, cells []tableCell, widths []int) {
	for i, cell := range cells {
		fmt.Fprint(w, cell.styled)
		if i < len(cells)-1 {
			padding := widths[i] - runewidth.StringWidth(cell.text) + 2
			fmt.Fprint(w, strings.Repeat(" ", padding))
		}
	}
	fmt.Fprintln(w)
}

func computeColumnWidths(headers []tableCell, rows [][]tableCell) []int {
	colWidths := make([]int, len(headers))
	for i, h := range headers {
		if w := runewidth.StringWidth(h.text); w > colWidths[i] {
			colWidths[i] = w
		}
	}
	for _, row := range rows {
		for i, cell := range row {
			if w := runewidth.StringWidth(cell.text); w > colWidths[i] {
				colWidths[i] = w
			}
		}
	}
	return colWidths
}

func getTerminalWidth() int {
	w, _, err := term.FromEnv().Size()
	if err != nil || w <= 0 {
		return 0
	}
	return w
}

func fitColumnsToTerminal(colWidths []int, flexibleCols []int, termWidth int) []int {
	if termWidth <= 0 {
		return colWidths
	}

	const colGap = 2
	const minFlexWidth = 10

	totalWidth := 0
	for i, w := range colWidths {
		totalWidth += w
		if i < len(colWidths)-1 {
			totalWidth += colGap
		}
	}

	overflow := totalWidth - termWidth
	if overflow <= 0 {
		return colWidths
	}

	result := make([]int, len(colWidths))
	copy(result, colWidths)

	for overflow > 0 {
		widestIdx := -1
		widestWidth := 0
		for _, idx := range flexibleCols {
			if result[idx] > widestWidth && result[idx] > minFlexWidth {
				widestWidth = result[idx]
				widestIdx = idx
			}
		}
		if widestIdx == -1 {
			break
		}
		result[widestIdx]--
		overflow--
	}

	return result
}

func truncateCells(rows [][]tableCell, colWidths []int, flexibleCols []int) [][]tableCell {
	flexSet := make(map[int]bool, len(flexibleCols))
	for _, idx := range flexibleCols {
		flexSet[idx] = true
	}

	for i, row := range rows {
		for j, cell := range row {
			if !flexSet[j] {
				continue
			}
			if runewidth.StringWidth(cell.text) > colWidths[j] {
				trimmed := trimText(cell.text, colWidths[j])
				rows[i][j] = cell.withText(trimmed)
			}
		}
	}
	return rows
}

func trimText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if limit <= 0 || runewidth.StringWidth(text) <= limit {
		return text
	}
	return runewidth.Truncate(text, limit, "...")
}

func formatRelativeTime(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	if now.Before(t) {
		return "0m"
	}
	age := now.Sub(t)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	case age < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	case age < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(age.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy", int(age.Hours()/(24*365)))
	}
}

func formatElapsed(status string, startedAt, updatedAt time.Time, now time.Time) string {
	if startedAt.IsZero() {
		return "-"
	}
	var end time.Time
	switch status {
	case "completed":
		end = updatedAt
	case "in_progress":
		end = now
	default:
		return "-"
	}
	if end.IsZero() || end.Before(startedAt) {
		return "-"
	}
	d := end.Sub(startedAt)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s > 0 {
			return fmt.Sprintf("%dm%ds", m, s)
		}
		return fmt.Sprintf("%dm", m)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dh", h)
}
