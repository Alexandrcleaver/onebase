package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ivantit66/onebase/internal/auth"
	"github.com/ivantit66/onebase/internal/navigation"
	"github.com/ivantit66/onebase/internal/storage"
)

// The authenticated login and parsed, permission-checked context alone choose
// the personal key. No login, layer, SQL key or raw delta is accepted from UI.
func (s *Server) personalNavigationAllowed(w http.ResponseWriter, r *http.Request, sub string) bool {
	user := auth.UserFromContext(r.Context())
	if user == nil || user.Login == "" {
		s.renderForbidden(w, r)
		return false
	}
	if s.store == nil {
		s.navigationEditorError(w, r, http.StatusServiceUnavailable, "Не удалось загрузить настройку меню")
		return false
	}
	if sub != "" {
		current := s.reg.GetSubsystem(sub)
		if current == nil {
			s.navigationEditorError(w, r, http.StatusBadRequest, "Не удалось загрузить настройку меню")
			return false
		}
		if !user.IsAdmin && !s.subsystemVisible(r, user, current) {
			s.renderForbidden(w, r)
			return false
		}
	}
	return true
}

func personalNavigationScope(r *http.Request, context string) storage.NavigationSettingsScope {
	return storage.NavigationSettingsScope{Layer: navigation.UserLayer, Login: currentUserLogin(r), Context: context}
}

// The full trees stay on the server. Base/Desired are the permitted editing
// surface, while runtime/preview retain the original full configuration base.
func (s *Server) personalNavigationState(r *http.Request, sub string) (navigationEditorState, error) {
	state, err := s.navigationEditorState(r, sub)
	if err != nil {
		return state, err
	}
	state.Personal = true
	state.configuration, state.layerBase = state.Base, state.Desired
	adminSetting := state.Setting
	state.Setting, err = s.store.GetNavigationSettings(r.Context(), personalNavigationScope(r, state.Base.Context))
	if err != nil {
		return state, err
	}
	var raw []byte
	if state.Setting.Exists {
		raw = []byte(state.Setting.Raw)
	}
	var diagnostics []navigation.Diagnostic
	state.effective, diagnostics = navigation.Compose(state.layerBase, nil, raw)
	state.Diagnostics = append(state.Diagnostics, diagnostics...)
	// IDs in stale rules can name now-forbidden metadata. Only generic codes
	// and counts are shown, never raw operations or diagnostic node identities.
	for i := range state.Diagnostics {
		if state.Diagnostics[i].Code != "invalid-layer" {
			state.Diagnostics[i].Node = ""
		}
		state.Diagnostics[i].Message = ""
	}
	state.Base = s.projectPersonalNavigation(r, state.layerBase, state, nil)
	baseNodes := navigationEditorNodes(state.Base)
	state.Desired = s.projectPersonalNavigation(r, state.effective, state, baseNodes)
	fullHash, _ := state.layerBase.Hash()
	visibleHash, _ := state.Base.Hash()
	revision := sha256.Sum256([]byte(fullHash + ":" + visibleHash))
	state.BaseRevision = hex.EncodeToString(revision[:])
	state.Renamed = []string{}
	state.Origins = map[string]string{}
	for id := range baseNodes {
		state.Origins[id] = "configuration"
	}
	mark := func(setting storage.NavigationSettings, layer navigation.Layer, base navigation.Tree, source string) navigation.Delta {
		if !setting.Exists {
			return navigation.Delta{}
		}
		delta, err := navigation.DecodeDelta([]byte(setting.Raw), layer)
		if err != nil {
			return navigation.Delta{}
		}
		if _, _, err := navigation.ApplyDelta(base, delta, layer); err != nil {
			return navigation.Delta{}
		}
		for _, op := range delta.Ops {
			id := op.Node
			if op.ID != "" {
				id = op.ID
			}
			if _, visible := baseNodes[id]; visible {
				state.Origins[id] = source
			}
		}
		return delta
	}
	mark(adminSetting, navigation.AdminLayer, state.configuration, "common")
	state.previous = mark(state.Setting, navigation.UserLayer, state.layerBase, "personal")
	desiredNodes := navigationEditorNodes(state.Desired)
	for id := range desiredNodes {
		if strings.HasPrefix(id, "usr:") {
			state.Origins[id] = "personal"
		}
	}
	for _, op := range state.previous.Ops {
		if op.Op == "rename" {
			if _, visible := desiredNodes[op.Node]; visible {
				if _, inherited := baseNodes[op.Node]; inherited {
					state.Renamed = append(state.Renamed, op.Node)
				}
			}
		}
	}
	return state, nil
}

// Keep empty personal folders, and inherited folders emptied by personal moves.
// Prune inherited ancestors containing only forbidden objects from the palette.
func (s *Server) projectPersonalNavigation(r *http.Request, tree navigation.Tree, state navigationEditorState, keep map[string]string) navigation.Tree {
	baseHash, _ := state.configuration.Hash()
	treeHash, _ := tree.Hash()
	semantic := state.Configured || baseHash != treeHash
	lang := s.resolveLang(r)
	empty := map[string]bool{}
	for index, source := range []navigation.Tree{state.configuration, state.layerBase} {
		for _, section := range source.Sections {
			if len(section.Items) == 0 && len(section.Groups) == 0 && (index == 0 || strings.HasPrefix(section.ID, "adm:")) {
				empty[section.ID] = true
			}
			for _, group := range section.Groups {
				if len(group.Items) == 0 && (index == 0 || strings.HasPrefix(group.ID, "adm:")) {
					empty[group.ID] = true
				}
			}
		}
	}
	items := func(input []navigation.Item) []navigation.Item {
		var result []navigation.Item
		for _, item := range input {
			resolved := navigation.ResolveItem(item, lang, "", func(key string) string { return s.tr(lang, key) }, state.Flat)
			if s.navigationItemVisible(r, item.Object, resolved, semantic, state.Flat) {
				result = append(result, item)
			}
		}
		return result
	}
	result := navigation.Tree{Version: tree.Version, Context: tree.Context, Sections: []navigation.Section{}}
	for _, section := range tree.Sections {
		section.Items = items(section.Items)
		var groups []navigation.Group
		for _, group := range section.Groups {
			group.Items = items(group.Items)
			if len(group.Items) > 0 || strings.HasPrefix(group.ID, "usr:") || keep[group.ID] != "" || empty[group.ID] {
				groups = append(groups, group)
			}
		}
		section.Groups = groups
		if len(section.Items) > 0 || len(section.Groups) > 0 || strings.HasPrefix(section.ID, "usr:") || keep[section.ID] != "" || empty[section.ID] {
			result.Sections = append(result.Sections, section)
		}
	}
	return result
}

// ID->editable title is also an inventory for rename intent. Objects and
// targets remain authoritative in navigation.Diff, never copied from clients.
func navigationEditorNodes(tree navigation.Tree) map[string]string {
	result := map[string]string{}
	for _, section := range tree.Sections {
		result[section.ID] = section.Title
		for _, item := range section.Items {
			result[item.ID] = item.Title
		}
		for _, group := range section.Groups {
			result[group.ID] = group.Title
			for _, item := range group.Items {
				result[item.ID] = item.Title
			}
		}
	}
	return result
}

// Replace editable intent while preserving valid previous intent for nodes
// currently outside RBAC. Re-diffing the full result removes stale references
// without turning the permitted projection into a persisted full snapshot.
func personalNavigationDelta(state navigationEditorState, input navigationEditorRequest) (navigation.Delta, navigation.Tree, error) {
	visible, err := navigation.Diff(state.Base, input.Desired, navigation.UserLayer)
	if err != nil {
		return navigation.Delta{}, navigation.Tree{}, err
	}
	editable := navigationEditorNodes(state.Base)
	for id, title := range navigationEditorNodes(state.Desired) {
		editable[id] = title
	}
	known := navigationEditorNodes(state.layerBase)
	for id, title := range navigationEditorNodes(state.effective) {
		known[id] = title
	}
	hash, _ := state.layerBase.Hash()
	combined := navigation.Delta{Version: 1, BaseHash: hash, Ops: []navigation.Operation{}}
	for _, op := range visible.Ops {
		if op.ID != "" {
			combined.Ops = append(combined.Ops, op)
		}
	}
	privateRenames := map[string]string{}
	for _, op := range state.previous.Ops {
		id := op.Node
		if op.ID != "" {
			id = op.ID
		}
		_, canEdit := editable[id]
		_, stillKnown := known[id]
		if !canEdit && stillKnown {
			combined.Ops = append(combined.Ops, op)
			if op.Op == "rename" {
				privateRenames[id] = *op.Title
			}
		}
	}
	for _, op := range visible.Ops {
		if op.ID == "" {
			combined.Ops = append(combined.Ops, op)
		}
	}
	desired, _, err := navigation.ApplyDelta(state.layerBase, combined, navigation.UserLayer)
	if err != nil {
		return navigation.Delta{}, navigation.Tree{}, err
	}
	delta, err := navigation.Diff(state.layerBase, desired, navigation.UserLayer)
	if err != nil {
		return navigation.Delta{}, navigation.Tree{}, err
	}
	// Explicitly typing the inherited title is still a personal override, even
	// when the current common title is equal. Preserve it across future renames.
	wanted := navigationEditorNodes(input.Desired)
	baseNodes := navigationEditorNodes(state.Base)
	seen := map[string]bool{}
	for _, id := range input.Renamed {
		_, inherited := baseNodes[id]
		title, present := wanted[id]
		if seen[id] || !inherited || !present {
			return navigation.Delta{}, navigation.Tree{}, errors.New("invalid rename intent")
		}
		seen[id] = true
		privateRenames[id] = title
	}
	for _, op := range delta.Ops {
		if op.Op == "rename" {
			delete(privateRenames, op.Node)
		}
	}
	ids := make([]string, 0, len(privateRenames))
	fullNodes := navigationEditorNodes(desired)
	for id := range privateRenames {
		if _, present := fullNodes[id]; present {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		title := privateRenames[id]
		delta.Ops = append(delta.Ops, navigation.Operation{Op: "rename", Node: id, Title: &title})
	}
	desired, _, err = navigation.ApplyDelta(state.layerBase, delta, navigation.UserLayer)
	if err == nil {
		_, err = navigation.EncodeDelta(delta, navigation.UserLayer)
	}
	return delta, desired, err
}

func (s *Server) personalNavigation(w http.ResponseWriter, r *http.Request) {
	sub := r.URL.Query().Get("subsystem")
	if !s.personalNavigationAllowed(w, r, sub) {
		return
	}
	state, err := s.personalNavigationState(r, sub)
	if err != nil {
		s.navigationEditorError(w, r, http.StatusServiceUnavailable, "Не удалось загрузить настройку меню")
		return
	}
	s.renderNavigationEditor(w, r, state, sub, "", http.StatusOK)
}

func (s *Server) personalNavigationWrite(w http.ResponseWriter, r *http.Request, preview bool) {
	if !s.personalNavigationAllowed(w, r, "") {
		return
	}
	input, err := readNavigationEditorRequestMode(w, r, true, true)
	if err != nil {
		s.navigationEditorError(w, r, http.StatusBadRequest, "Некорректная структура меню")
		return
	}
	if !s.personalNavigationAllowed(w, r, input.Subsystem) {
		return
	}
	state, err := s.personalNavigationState(r, input.Subsystem)
	if err != nil {
		s.navigationEditorError(w, r, http.StatusServiceUnavailable, "Не удалось загрузить настройку меню")
		return
	}
	if input.Revision != state.Setting.Revision || input.BaseRevision != state.BaseRevision {
		s.personalNavigationConflict(w, r, state, input.Subsystem)
		return
	}
	if err := allocateNavigationContainersForLayer(state.Base, state.Desired, &input.Desired, navigation.UserLayer); err != nil {
		s.navigationEditorError(w, r, http.StatusBadRequest, "Некорректная структура меню")
		return
	}
	delta, desired, err := personalNavigationDelta(state, input)
	if err != nil {
		s.navigationEditorError(w, r, http.StatusBadRequest, "Некорректная структура меню")
		return
	}
	if preview {
		state.effective = desired
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"preview": s.editorNavigationPreview(r, state, input.Subsystem)})
		return
	}
	next, err := s.store.SaveNavigationSettings(r.Context(), personalNavigationScope(r, state.Base.Context), state.layerBase, delta, input.Revision)
	if errors.Is(err, storage.ErrVersionConflict) {
		fresh, err := s.personalNavigationState(r, input.Subsystem)
		if err != nil {
			s.navigationEditorError(w, r, http.StatusServiceUnavailable, "Не удалось загрузить настройку меню")
			return
		}
		s.personalNavigationConflict(w, r, fresh, input.Subsystem)
		return
	}
	if err != nil {
		s.navigationEditorError(w, r, http.StatusInternalServerError, "Не удалось сохранить настройку меню")
		return
	}
	s.auditNavigation(r, "navigation.user.save", state.Base.Context, state.Setting.Revision, next.Revision, len(delta.Ops))
	s.personalNavigationRedirect(w, r, input.Subsystem)
}

func (s *Server) personalNavigationSave(w http.ResponseWriter, r *http.Request) {
	s.personalNavigationWrite(w, r, false)
}

func (s *Server) personalNavigationPreview(w http.ResponseWriter, r *http.Request) {
	s.personalNavigationWrite(w, r, true)
}

func (s *Server) personalNavigationReset(w http.ResponseWriter, r *http.Request) {
	if !s.personalNavigationAllowed(w, r, "") {
		return
	}
	input, err := readNavigationEditorRequestMode(w, r, false, true)
	if err != nil {
		s.navigationEditorError(w, r, http.StatusBadRequest, "Некорректная структура меню")
		return
	}
	if !s.personalNavigationAllowed(w, r, input.Subsystem) {
		return
	}
	state, err := s.personalNavigationState(r, input.Subsystem)
	if err != nil {
		s.navigationEditorError(w, r, http.StatusServiceUnavailable, "Не удалось загрузить настройку меню")
		return
	}
	next, err := s.store.DeleteNavigationSettings(r.Context(), personalNavigationScope(r, state.Base.Context), input.Revision)
	if errors.Is(err, storage.ErrVersionConflict) {
		s.personalNavigationConflict(w, r, state, input.Subsystem)
		return
	}
	if err != nil {
		s.navigationEditorError(w, r, http.StatusInternalServerError, "Не удалось сохранить настройку меню")
		return
	}
	s.auditNavigation(r, "navigation.user.reset", state.Base.Context, state.Setting.Revision, next.Revision, 0)
	s.personalNavigationRedirect(w, r, input.Subsystem)
}

func (s *Server) personalNavigationConflict(w http.ResponseWriter, r *http.Request, state navigationEditorState, sub string) {
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"revision": state.Setting.Revision, "baseRevision": state.BaseRevision, "preview": s.editorNavigationPreview(r, state, sub)})
		return
	}
	s.renderNavigationEditor(w, r, state, sub, s.tr(s.resolveLang(r), "Меню изменилось в другой вкладке. Загрузите актуальную версию и повторите изменения."), http.StatusConflict)
}

func (s *Server) personalNavigationRedirect(w http.ResponseWriter, r *http.Request, sub string) {
	query := url.Values{"saved": {"1"}}
	if sub != "" {
		query.Set("subsystem", sub)
	}
	http.Redirect(w, r, "/ui/settings/navigation?"+query.Encode(), http.StatusSeeOther)
}
