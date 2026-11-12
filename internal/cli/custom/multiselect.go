// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package custom

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/google-gemini/gemini-api-cli/internal/forms"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// multiSelect asks the user to tick options: a redrawing list on a terminal,
// a numbered line prompt elsewhere. The generated forms have no multi-select.
type multiSelect struct {
	title    string
	options  []string
	selected []bool // defaults in, answer out
	single   bool   // exactly one: the selection follows the cursor

	cursor   int
	finished bool
	aborted  bool
}

// Colours of the generated forms.
var (
	listAccent = lipgloss.Color("#38BDF8")
	listDimmed = lipgloss.Color("#64748B")
)

func (m *multiSelect) run(in io.Reader, out io.Writer, linePrompts bool) error {
	if linePrompts || os.Getenv("TERM") == "dumb" {
		return m.ask(in, out)
	}
	if _, err := tea.NewProgram(m, tea.WithOutput(os.Stderr)).Run(); err != nil && !errors.Is(err, tea.ErrInterrupted) {
		return err
	}
	if !m.finished {
		return forms.ErrUserAborted
	}
	return nil
}

func (m *multiSelect) Init() tea.Cmd {
	for i, on := range m.selected {
		if m.single && on {
			m.cursor = i
		}
	}
	return nil
}

func (m *multiSelect) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	n := len(m.options)
	switch key.String() {
	case "ctrl+c":
		m.aborted = true
		return m, tea.Quit
	case "enter":
		m.finished = true
		return m, tea.Quit
	case "up", "k":
		m.cursor = (m.cursor - 1 + n) % n
	case "down", "j":
		m.cursor = (m.cursor + 1) % n
	case "space", "x":
		m.selected[m.cursor] = !m.selected[m.cursor]
	case "a":
		all := m.count() == len(m.selected)
		for i := range m.selected {
			m.selected[i] = !all
		}
	}
	if m.single {
		for i := range m.selected {
			m.selected[i] = i == m.cursor
		}
	}
	return m, nil
}

func (m *multiSelect) count() int {
	n := 0
	for _, on := range m.selected {
		if on {
			n++
		}
	}
	return n
}

func mark(on bool) string {
	if on {
		return "x"
	}
	return " "
}

func (m *multiSelect) View() tea.View {
	if m.aborted {
		return tea.NewView("")
	}
	// The confirmed list stays on screen at the same height: the renderer
	// leaves stale rows behind when a frame gets shorter.
	focus := lipgloss.NewStyle().Foreground(listAccent).Bold(true)
	footer := "space toggle · a all/none · enter confirm"
	if m.single {
		footer = "up/down move · enter confirm"
	}
	if m.finished {
		focus = lipgloss.NewStyle().Foreground(listDimmed)
		footer = fmt.Sprintf("%d selected", m.count())
	}
	box := "[%s] "
	if m.single {
		box = "(%s) "
	}
	lines := []string{focus.Render(m.title)}
	for i, option := range m.options {
		line := fmt.Sprintf(box, mark(m.selected[i])) + option
		if i == m.cursor && !m.finished {
			lines = append(lines, focus.Render("> "+line))
		} else {
			lines = append(lines, "  "+line)
		}
	}
	list := lipgloss.NewStyle().
		BorderStyle(lipgloss.ThickBorder()).BorderLeft(true).BorderForeground(focus.GetForeground()).
		PaddingLeft(1).
		Render(strings.Join(lines, "\n"))
	return tea.NewView(list + "\n" + lipgloss.NewStyle().Foreground(listDimmed).Render("  "+footer) + "\n")
}

// ask is the line prompt. A blank answer or end of input keeps the defaults.
func (m *multiSelect) ask(in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, m.title)
	var defaults []string
	for i, option := range m.options {
		if m.selected[i] {
			defaults = append(defaults, strconv.Itoa(i+1))
		}
		fmt.Fprintf(out, "  %2d. [%s] %s\n", i+1, mark(m.selected[i]), option)
	}
	hint := `Numbers separated by commas, "all" or "none"`
	if m.single {
		hint = "One number"
	}
	for {
		fmt.Fprintf(out, "%s [%s]: ", hint, strings.Join(defaults, ","))
		line, err := readLine(in)
		fmt.Fprintln(out)
		if err != nil && err != io.EOF {
			return err
		}
		if m.parse(line) {
			return nil
		}
		problem := fmt.Sprintf("Enter numbers from 1 to %d.", len(m.options))
		if err == io.EOF {
			return errors.New(problem)
		}
		fmt.Fprintln(out, problem)
	}
}

// parse applies a line-prompt answer and reports whether it was valid.
func (m *multiSelect) parse(line string) bool {
	answer := strings.ToLower(strings.TrimSpace(line))
	picked := make([]bool, len(m.options))
	count := 0
	switch answer {
	case "":
		return true
	case "none":
	case "all":
		for i := range picked {
			picked[i] = true
		}
		count = len(picked)
	default:
		for _, part := range strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' }) {
			n, err := strconv.Atoi(part)
			if err != nil || n < 1 || n > len(picked) {
				return false
			}
			picked[n-1] = true
			count++
		}
	}
	if m.single && count != 1 {
		return false
	}
	m.selected = picked
	return true
}

// readLine reads a byte at a time, leaving the rest of in for the next prompt.
func readLine(in io.Reader) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n == 1 && buf[0] == '\n' {
			return strings.TrimSuffix(string(line), "\r"), nil
		}
		line = append(line, buf[:n]...)
		if err != nil {
			return string(line), err
		}
	}
}
