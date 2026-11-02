package store

import (
	"errors"
	"reflect"
	"testing"

	"poligon/environment/internal/domain"
)

func TestTransactionRollsBackAllStagedChanges(t *testing.T) {
	sess := NewManager().GetOrCreate("s")
	before := sess.GetSnapshot()
	wantErr := errors.New("abort")
	err := sess.Transact(func(tx *Transaction) error {
		if _, _, err := tx.ReserveSlot("slot-101", "o"); err != nil {
			t.Fatal(err)
		}
		tx.SaveOrder(domain.Order{ID: "o", SlotID: "slot-101", Status: domain.OrderStatusActive})
		tx.AppendAudit(domain.AuditEvent{Action: domain.AuditActionCreateOrder})
		return wantErr
	})
	if !errors.Is(err, wantErr) || !reflect.DeepEqual(before, sess.GetSnapshot()) || len(sess.GetAuditLog()) != 0 {
		t.Fatal("aborted transaction changed session state or audit")
	}
}

func TestInitFixturesDistinguishesMissingAndEmpty(t *testing.T) {
	m := NewManager()
	defaults := m.InitSession("default-fixtures", nil, nil, nil).GetSnapshot()
	if len(defaults.Clients) == 0 || len(defaults.Slots) == 0 {
		t.Fatal("missing fixtures did not use defaults")
	}
	empty := m.InitSession("empty", []domain.Client{}, []domain.Slot{}, nil).GetSnapshot()
	if len(empty.Clients) != 0 || len(empty.Slots) != 0 {
		t.Fatal("explicitly empty fixtures were replaced")
	}
	partial := m.InitSession("partial", []domain.Client{{ID: "custom"}}, nil, nil).GetSnapshot()
	if len(partial.Clients) != 1 || partial.Clients["custom"].ID != "custom" || len(partial.Slots) == 0 {
		t.Fatal("partial fixtures did not preserve custom clients and default slots")
	}
}
