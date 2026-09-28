package query_test

// Ссылка на учётную запись входа (type: reference:_users, #1646) в языке
// запросов. Системная таблица _users сознательно не регистрируется источником
// прав, поэтому через точку читается только объявленное: Ссылка, Наименование,
// Логин и ПолноеИмя. Раньше любое имя после точки уходило в SQL дословно, и
// «Автор.password_hash» отдавал хеш пароля любому запросу — отчёту, виджету,
// ИИ-помощнику не-администратора.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ivantit66/onebase/internal/auth"
	"github.com/ivantit66/onebase/internal/dbtest"
	"github.com/ivantit66/onebase/internal/metadata"
	"github.com/ivantit66/onebase/internal/query"
	"github.com/ivantit66/onebase/internal/storage"
)

func usersRefEntity() *metadata.Entity {
	return &metadata.Entity{
		Name: "Заявка",
		Kind: metadata.KindDocument,
		Fields: []metadata.Field{
			{Name: "Номер", Type: metadata.FieldTypeString},
			{Name: "Автор", Type: "reference:_users", RefEntity: metadata.SystemUsersEntity},
		},
	}
}

func TestUsersRefNavigationReadsDeclaredAttributes(t *testing.T) {
	dbtest.ForEachDialect(t, func(t *testing.T, db *storage.DB) {
		ctx := context.Background()
		repo := auth.NewRepo(db)
		if err := repo.EnsureSchema(ctx); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
		ivanov, err := repo.Create(ctx, "ivanov", "пароль-123456", "Иванов И.И.", true)
		if err != nil {
			t.Fatalf("Create ivanov: %v", err)
		}
		// Без полного имени представление учётки — логин.
		petrov, err := repo.Create(ctx, "petrov", "пароль-654321", "", false)
		if err != nil {
			t.Fatalf("Create petrov: %v", err)
		}
		заявка := usersRefEntity()
		if err := db.Migrate(ctx, []*metadata.Entity{заявка}); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		for номер, автор := range map[string]string{"0001": ivanov.ID, "0002": petrov.ID} {
			if err := db.Upsert(ctx, заявка.Name, uuid.New(), map[string]any{"Номер": номер, "Автор": автор}, заявка); err != nil {
				t.Fatalf("Upsert %s: %v", номер, err)
			}
		}

		compiled, err := query.Compile(`
			ВЫБРАТЬ Номер, Автор.Логин, Автор.ПолноеИмя, Автор.Наименование КАК Имя, Автор.Ссылка КАК Ид
			ИЗ Документ.Заявка
			ГДЕ Автор.Логин <> ""
			УПОРЯДОЧИТЬ ПО Номер`, query.CompileOpts{Dialect: db.Dialect(), Entities: []*metadata.Entity{заявка}})
		if err != nil {
			t.Fatalf("компиляция: %v", err)
		}
		rows, _, err := query.Run(ctx, db, &compiled)
		if err != nil {
			t.Fatalf("выполнение: %v\nSQL: %s", err, compiled.SQL)
		}
		if len(rows) != 2 {
			t.Fatalf("строк %d, ожидалось 2: %v", len(rows), rows)
		}
		want := []struct{ login, fullName, name, id string }{
			{"ivanov", "Иванов И.И.", "Иванов И.И.", ivanov.ID},
			{"petrov", "", "petrov", petrov.ID},
		}
		for i, w := range want {
			row := rows[i]
			if got := fmt.Sprint(row["логин"]); got != w.login {
				t.Errorf("строка %d: Логин = %q, want %q (row %v)", i, got, w.login, row)
			}
			if got := fmt.Sprint(row["полноеимя"]); got != w.fullName {
				t.Errorf("строка %d: ПолноеИмя = %q, want %q", i, got, w.fullName)
			}
			if got := fmt.Sprint(row["имя"]); got != w.name {
				t.Errorf("строка %d: Наименование = %q, want %q", i, got, w.name)
			}
			if got := usersRefID(row["ид"]); got != w.id {
				t.Errorf("строка %d: Ссылка = %q, want %q", i, got, w.id)
			}
		}

		// Квалифицированный путь и сортировка по представлению тоже исполнимы.
		ordered, err := query.Compile(`
			ВЫБРАТЬ З.Номер, З.Автор.Логин КАК Л
			ИЗ Документ.Заявка КАК З
			УПОРЯДОЧИТЬ ПО З.Автор.Наименование`, query.CompileOpts{Dialect: db.Dialect(), Entities: []*metadata.Entity{заявка}})
		if err != nil {
			t.Fatalf("компиляция квалифицированного пути: %v", err)
		}
		if _, _, err := query.Run(ctx, db, &ordered); err != nil {
			t.Fatalf("выполнение квалифицированного пути: %v\nSQL: %s", err, ordered.SQL)
		}
	})
}

func TestUsersRefNavigationRejectsAuthColumns(t *testing.T) {
	заявка := usersRefEntity()
	for _, text := range []string{
		`ВЫБРАТЬ Автор.password_hash ИЗ Документ.Заявка`,
		`ВЫБРАТЬ Автор.totp_secret ИЗ Документ.Заявка`,
		`ВЫБРАТЬ Автор.is_admin ИЗ Документ.Заявка`,
		`ВЫБРАТЬ Автор.full_name ИЗ Документ.Заявка`,
		`ВЫБРАТЬ З.Автор.password_hash КАК Х ИЗ Документ.Заявка КАК З`,
		`ВЫБРАТЬ Номер ИЗ Документ.Заявка ГДЕ Автор.password_hash ПОДОБНО "$2%"`,
		`ВЫБРАТЬ Номер ИЗ Документ.Заявка УПОРЯДОЧИТЬ ПО Автор.auth_subject`,
	} {
		for _, dialect := range []storage.Dialect{storage.SQLiteDialect{}, storage.PgDialect{}} {
			res, err := query.Compile(text, query.CompileOpts{Dialect: dialect, Entities: []*metadata.Entity{заявка}})
			if err == nil {
				t.Errorf("%s (%s): запрос скомпилирован, ожидался отказ\nSQL: %s", text, dialect.Name(), res.SQL)
				continue
			}
			if !strings.Contains(err.Error(), "недоступно") {
				t.Errorf("%s (%s): ошибка не называет причину: %v", text, dialect.Name(), err)
			}
		}
	}
}

func usersRefID(v any) string {
	switch id := v.(type) {
	case [16]byte:
		return uuid.UUID(id).String()
	case uuid.UUID:
		return id.String()
	case []byte:
		return string(id)
	}
	return fmt.Sprint(v)
}
