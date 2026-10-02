package ui

import "github.com/ivantit66/onebase/internal/navigation"

// navigationEditorRequest carries a desired layout, never a SQL key or a delta.
// The administrator's handler selects the scope and derives changes on the server.
type navigationEditorRequest struct {
	Subsystem string          `json:"subsystem"`
	Revision  string          `json:"revision"`
	Desired   navigation.Tree `json:"desired"`
}
