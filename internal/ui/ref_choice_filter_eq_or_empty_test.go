package ui

import (
	"testing"

	"github.com/google/uuid"
	"github.com/ivantit66/onebase/internal/metadata"
)

// Пустой источник у eq закрывает подбор (fail closed), а у eq_or_empty —
// оставляет записи с пустым реквизитом: запись без филиала доступна и в
// документе, где филиал ещё не выбран (как список отбора 1С из пустой ссылки).
func TestChoicePredicatesEqualOrEmptyEmptySource(t *testing.T) {
	element := func(op metadata.FormChoiceOperator) *metadata.FormElement {
		return &metadata.FormElement{
			ID: "ad", Name: "ПолеРеклама", Kind: metadata.FormElementField, DataPath: "Объект.Реклама",
			ChoiceFilter: []metadata.FormChoiceCondition{{Field: "Филиал", Op: op, From: "Объект.Филиал"}},
		}
	}
	empty := map[string]string{"Объект.Филиал": ""}

	predicates, closed, err := choicePredicates(element(metadata.FormChoiceOpEqualOrEmpty), empty)
	if err != nil {
		t.Fatalf("eq_or_empty: %v", err)
	}
	if closed || len(predicates) != 1 || predicates[0].Value != nil {
		t.Fatalf("eq_or_empty with empty source: closed=%v predicates=%+v, want one predicate with nil value", closed, predicates)
	}

	if _, closed, err := choicePredicates(element(metadata.FormChoiceOpEqual), empty); err != nil || !closed {
		t.Fatalf("eq with empty source: closed=%v err=%v, want fail-closed", closed, err)
	}

	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	predicates, closed, err = choicePredicates(element(metadata.FormChoiceOpEqualOrEmpty), map[string]string{"Объект.Филиал": id.String()})
	if err != nil || closed || len(predicates) != 1 || predicates[0].Value != id {
		t.Fatalf("eq_or_empty with source: closed=%v err=%v predicates=%+v", closed, err, predicates)
	}
}
