// Package projects discovers and opens local project directories, with GitHub
// clone-on-miss for projects not yet checked out.
package projects

import "fmt"

// Project is a single entry in the project list.
type Project struct {
	Name   string
	Dir    string
	Status GitStatus
}

// GitStatus summarises the working-tree state of a project directory.
type GitStatus struct {
	Modified  int
	Untracked int
	Ahead     int
}

// Label returns a short human-readable badge: "[clean]", "[2 ahead]",
// "[3 modified]", etc. Returns "" for non-git directories.
func (gs GitStatus) Label() string {
	if gs.Modified == 0 && gs.Untracked == 0 && gs.Ahead == 0 {
		return "[clean]"
	}
	if gs.Ahead > 0 && gs.Modified == 0 && gs.Untracked == 0 {
		return fmt.Sprintf("[%d ahead]", gs.Ahead)
	}
	var parts string
	if gs.Modified > 0 {
		parts = fmt.Sprintf("%d modified", gs.Modified)
	}
	if gs.Untracked > 0 {
		if parts != "" {
			parts += ", "
		}
		parts += fmt.Sprintf("%d untracked", gs.Untracked)
	}
	return "[" + parts + "]"
}

// DisplayLine builds the label shown in the interactive picker.
func (p Project) DisplayLine() string {
	label := p.Status.Label()
	if label == "" {
		return p.Name
	}
	return p.Name + " " + label
}
