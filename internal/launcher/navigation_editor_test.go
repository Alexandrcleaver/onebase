package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/ivantit66/onebase/internal/configdb"
	"github.com/ivantit66/onebase/internal/i18n"
	"github.com/ivantit66/onebase/internal/metadata"
	"gopkg.in/yaml.v3"
)

const navigationEditorFixture = `# school configuration
name: School
title: School
titles:
  en: School in English
# unrelated permissions
roles: [Teacher]
future_flag: preserve
contents:
  catalogs: [B, A]
  documents: [Order]
home_page:
  title: Dashboard
  future_home: keep
menu:
  # section comment
  sections:
    - id: education
      title: Education
      titles:
        en: English education
      items:
        - id: a
          target: catalog:A
        - id: order
          target: document:Order
      groups:
        - id: year
          title: Year
          items:
            # item B travels with this comment
            - id: b
              target: catalog:B
`

func newNavigationEditorFixture(t *testing.T, mode, subPath string) (*handler, *Base) {
	t.Helper()
	bundle, err := i18n.Load(i18n.EmbeddedLocales, "")
	if err != nil {
		t.Fatal(err)
	}
	previous := launcherBundle
	launcherBundle = bundle
	t.Cleanup(func() { launcherBundle = previous })
	store := newTestStore(t)
	b := &Base{ID: "test", Name: "Test", ConfigSource: mode, Path: t.TempDir(), DBType: "sqlite", DBPath: filepath.Join(t.TempDir(), "base.db")}
	if err := store.Add(b); err != nil {
		t.Fatal(err)
	}
	h := &handler{store: store, runner: NewRunner()}
	files := []configdb.ConfigFile{
		{Path: "config/app.yaml", Content: []byte("name: Test\n")},
		{Path: "catalogs/a.yaml", Content: []byte("name: A\nfields: []\n")},
		{Path: "catalogs/b.yaml", Content: []byte("name: B\nfields: []\n")},
		{Path: "documents/order.yaml", Content: []byte("name: Order\nfields: []\n")},
		{Path: subPath, Content: []byte(navigationEditorFixture)},
		{Path: "tree_order.yaml", Content: []byte("catalogs: [A, B]\n")},
		{Path: "config/home_page.yaml", Content: []byte("# dashboard comment\ntitle: Home\ntitles:\n  en: English home\nfuture_home: preserve\nnav:\n  catalogs: [A]\nmenu:\n  sections:\n    - id: home\n      title: Home\n      items:\n        - id: home-a\n          target: catalog:A\n")},
	}
	if mode == "database" {
		db, err := OpenDB(context.Background(), b)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		repo := configdb.New(db)
		if err := repo.EnsureSchema(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := repo.SaveFiles(context.Background(), files, configdb.VersionOptions{Message: "fixture"}); err != nil {
			t.Fatal(err)
		}
	} else {
		for _, file := range files {
			full := filepath.Join(b.Path, filepath.FromSlash(file.Path))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, file.Content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return h, b
}

// Exercise HTTP routing and JSON boundaries, not a private YAML transformer.
func navigationEditorHTTP(h *handler, method, endpoint, body string) *httptest.ResponseRecorder {
	router := chi.NewRouter()
	router.Get("/bases/{id}/configurator/navigation", h.configuratorNavigation)
	router.Post("/bases/{id}/configurator/navigation/save", h.configuratorNavigationSave)
	router.Post("/bases/{id}/configurator/navigation/preview", h.configuratorNavigationPreview)
	r := httptest.NewRequest(method, "/bases/test/configurator/navigation"+endpoint, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)
	return rec
}

func readNavigationEditorData(t *testing.T, rec *httptest.ResponseRecorder) navigationEditorData {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var data navigationEditorData
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func navigationEditorBody(t *testing.T, sub string, menu *metadata.Menu) string {
	t.Helper()
	raw, err := json.Marshal(navigationEditorRequest{Subsystem: sub, Menu: menu, Lang: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestNavigationEditorHTTPStorageRoundTrip(t *testing.T) {
	var fileResult []byte
	for _, mode := range []string{"file", "database"} {
		t.Run(mode, func(t *testing.T) {
			const subPath = "subsystems/SchoolCustom.yaml"
			h, b := newNavigationEditorFixture(t, mode, subPath)
			data := readNavigationEditorData(t, navigationEditorHTTP(h, http.MethodGet, "?subsystem=School", ""))
			if len(data.Palette) != 3 || data.Menu.Sections[0].ID != "education" {
				t.Fatalf("incorrect editor context: %+v", data)
			}
			s := &data.Menu.Sections[0]
			s.Title, s.Icon = "Lessons", "book-open"
			bItem := s.Groups[0].Items[0]
			s.Groups = []metadata.MenuGroup{{ID: "mixed", Title: "School year", Icon: "folder", Items: []metadata.MenuItem{bItem, s.Items[1]}}}
			s.Items = nil // A is unplaced and must return to Other.
			body := navigationEditorBody(t, "School", data.Menu)
			preview := readNavigationEditorData(t, navigationEditorHTTP(h, http.MethodPost, "/preview", body))
			if len(preview.Preview) != 2 || preview.Preview[0].Title != "Lessons" || len(preview.Preview[0].Groups[0].Items) != 2 || preview.Preview[1].ID != "cfg:other" || preview.Preview[1].Items[0].Label != "A" {
				t.Fatalf("runtime preview lost mixed group or Other: %+v", preview.Preview)
			}
			before, _ := h.readConfigFileRaw(context.Background(), b, subPath)
			if string(before) != navigationEditorFixture {
				t.Fatal("preview wrote configuration")
			}
			readNavigationEditorData(t, navigationEditorHTTP(h, http.MethodPost, "/save", body))
			got, ok := h.readConfigFileRaw(context.Background(), b, subPath)
			if !ok {
				t.Fatal("source file disappeared")
			}
			for _, text := range []string{"# school configuration", "# unrelated permissions", "roles: [Teacher]", "future_flag: preserve", "future_home: keep", "catalogs: [B, A]", "en: English education", "# section comment", "# item B travels with this comment", "id: mixed", "title: Lessons", "icon: book-open"} {
				if !strings.Contains(string(got), text) {
					t.Errorf("lost %q:\n%s", text, got)
				}
			}
			if _, exists := h.readConfigFileRaw(context.Background(), b, "subsystems/school.yaml"); exists {
				t.Fatal("save created a second file derived from the object name")
			}
			var saved struct {
				Menu *metadata.Menu `yaml:"menu"`
			}
			if err := yaml.Unmarshal(got, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.Menu.Sections[0].Groups[0].Items[0].ID != "b" || saved.Menu.Sections[0].Titles["en"] != "English education" {
				t.Fatal("move changed identity or translations")
			}
			if mode == "file" {
				fileResult = got
			} else if !bytes.Equal(fileResult, got) {
				t.Fatalf("file/database YAML differs:\nfile:\n%s\ndatabase:\n%s", fileResult, got)
			}
		})
	}
}

func TestNavigationEditorImportIsReadOnlyAndPreservesContentsOrder(t *testing.T) {
	for _, mode := range []string{"file", "database"} {
		t.Run(mode, func(t *testing.T) {
			h, b := newNavigationEditorFixture(t, mode, "subsystems/school.yaml")
			for _, kind := range []string{"legacy", "tree-order"} {
				data := readNavigationEditorData(t, navigationEditorHTTP(h, http.MethodGet, "?subsystem=School&import="+kind, ""))
				if data.Error != "" || data.Menu.Sections[0].Items[0].Target != "catalog:B" || data.Menu.Sections[0].Items[1].Target != "catalog:A" {
					t.Fatalf("import ignored explicit contents order: %+v", data)
				}
				raw, _ := h.readConfigFileRaw(context.Background(), b, "subsystems/school.yaml")
				if string(raw) != navigationEditorFixture {
					t.Fatal("import wrote menu before Save")
				}
			}
			global := readNavigationEditorData(t, navigationEditorHTTP(h, http.MethodGet, "", ""))
			if len(global.Palette) != 1 || global.Palette[0].Target != "catalog:A" {
				t.Fatalf("global scoped nav expanded membership: %+v", global.Palette)
			}
		})
	}
}

func TestNavigationEditorBadBodiesDoNotWrite(t *testing.T) {
	for _, mode := range []string{"file", "database"} {
		t.Run(mode, func(t *testing.T) {
			h, b := newNavigationEditorFixture(t, mode, "subsystems/school.yaml")
			cases := []string{
				"{", "null", "{}", "subsystem=School&menu=bad", "{} {}",
				`{"subsystem":"School","menu":{"sections":[]},"roles":["Admin"]}`,
				`{"subsystem":"School","menu":{"sections":[{"id":"x","title":"X","items":[{"id":"bad","target":"catalog:Outside"}]}]}}`,
				`{"subsystem":"School","menu":{"sections":[{"id":"x","title":"X","groups":[{"id":"y","title":"Y","groups":[]}]}]}}`,
				strings.Repeat(" ", maxNavigationEditorBody+1),
				string([]byte{'{', '"', 0xff, '"', ':', '1', '}'}),
			}
			tooMany := &metadata.Menu{}
			for i := 0; i <= 100; i++ {
				tooMany.Sections = append(tooMany.Sections, metadata.MenuSection{ID: fmt.Sprintf("s-%d", i), Title: "Section"})
			}
			cases = append(cases, navigationEditorBody(t, "School", tooMany))
			for i, body := range cases {
				rec := navigationEditorHTTP(h, http.MethodPost, "/save", body)
				if rec.Code < 400 {
					t.Errorf("case %d accepted malformed/oversize input: %s", i, rec.Body.String())
				}
				raw, _ := h.readConfigFileRaw(context.Background(), b, "subsystems/school.yaml")
				if string(raw) != navigationEditorFixture {
					t.Fatalf("case %d changed YAML", i)
				}
			}
		})
	}
}
