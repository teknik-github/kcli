package config

import (
	"os"
	"path/filepath"
	"sort"

	"sigs.k8s.io/yaml"
)

// Workspaces are saved layouts: the open tabs and the split arrangement, so a
// monitoring setup survives a restart. They live in their own file rather than
// the user config because kcli writes them — the config file stays something
// only the user edits.
//
// The file is best-effort in both directions: unreadable or malformed means "no
// saved workspaces", and a failed save is reported to the user but never fatal.

// WorkspaceFileName is the file workspaces are stored in, beside config.yaml.
const WorkspaceFileName = "workspaces.yaml"

// DefaultWorkspace is restored at startup when it exists — saving under this
// name is how a user opts in to a startup layout.
const DefaultWorkspace = "default"

// LastWorkspace is overwritten on quit so an accidentally closed layout can be
// recovered with ":ws load last".
const LastWorkspace = "last"

// Tab is one saved view session. Only what a session is *about* is stored (the
// resource, where it points, how it is filtered) — never fetched rows.
type Tab struct {
	Name      string `json:"name,omitempty"`   // user label; empty = auto title
	View      string `json:"view"`             // viewDef.ID ("pod", "deployment", …)
	Namespace string `json:"namespace"`        // "" = all namespaces
	Filter    string `json:"filter,omitempty"` // active row filter
	SortCol   int    `json:"sortCol"`          // -1 = fetch order
	SortDesc  bool   `json:"sortDesc,omitempty"`

	// A tab parked on a CRD (the Dynamic view) records the GVR it points at, so
	// it can be re-pointed on restore without another discovery round-trip.
	Group      string `json:"group,omitempty"`
	Version    string `json:"version,omitempty"`
	Resource   string `json:"resource,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespaced bool   `json:"namespaced,omitempty"`
}

// Workspace is one saved layout.
type Workspace struct {
	Tabs       []Tab `json:"tabs"`
	ActiveTab  int   `json:"activeTab"`
	Split      int   `json:"split,omitempty"`      // 0 off, 1 columns, 2 stacked, 3 grid
	ActivePane int   `json:"activePane,omitempty"` // pane position holding the active tab
	PaneTabs   []int `json:"paneTabs,omitempty"`   // tab index per pane position (2..4 entries)
}

// workspaceFile mirrors the on-disk YAML: a map of name -> layout.
type workspaceFile struct {
	Workspaces map[string]Workspace `json:"workspaces"`
}

// WorkspacePath returns the workspace file path, beside the config file.
func WorkspacePath() string {
	if p := Path(); p != "" {
		return filepath.Join(filepath.Dir(p), WorkspaceFileName)
	}
	return ""
}

// LoadWorkspaces reads every saved workspace. A missing or malformed file
// yields an empty map, never an error — the same rule as the config file.
func LoadWorkspaces() map[string]Workspace {
	path := WorkspacePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f workspaceFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil
	}
	return f.Workspaces
}

// LoadWorkspace returns one saved workspace by name.
func LoadWorkspace(name string) (Workspace, bool) {
	w, ok := LoadWorkspaces()[name]
	return w, ok
}

// SaveWorkspace stores ws under name, keeping the other saved workspaces. It
// creates the config directory when needed. Unlike loading, failures are
// returned: the user asked for this one, so they should hear if it did not work.
func SaveWorkspace(name string, ws Workspace) error {
	path := WorkspacePath()
	if path == "" {
		return os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	all := LoadWorkspaces()
	if all == nil {
		all = map[string]Workspace{}
	}
	all[name] = ws
	data, err := yaml.Marshal(workspaceFile{Workspaces: all})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// DeleteWorkspace removes a saved workspace. Removing one that isn't there is
// not an error.
func DeleteWorkspace(name string) error {
	all := LoadWorkspaces()
	if _, ok := all[name]; !ok {
		return nil
	}
	delete(all, name)
	path := WorkspacePath()
	data, err := yaml.Marshal(workspaceFile{Workspaces: all})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// WorkspaceNames lists the saved workspaces, sorted.
func WorkspaceNames() []string {
	all := LoadWorkspaces()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
