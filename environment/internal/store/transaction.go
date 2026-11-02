package store

import (
	"fmt"
	"time"

	"poligon/environment/internal/domain"
)

// Transaction stages order, slot and audit changes under the session lock.
// It must only be used within the Transact callback.
type Transaction struct {
	session *SessionState
	orders  map[string]domain.Order
	slots   map[string]domain.Slot
	audit   []domain.AuditEvent
}

// Transact commits all staged changes together, or discards them on error.
// Snapshot readers and Reset use the same lock and cannot observe partial changes.
func (s *SessionState) Transact(fn func(*Transaction) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx := &Transaction{
		session: s,
		orders:  make(map[string]domain.Order),
		slots:   make(map[string]domain.Slot),
	}
	if err := fn(tx); err != nil {
		return err
	}
	for id, order := range tx.orders {
		s.Orders[id] = order
	}
	for id, slot := range tx.slots {
		s.Slots[id] = slot
	}
	if len(tx.orders) > 0 || len(tx.slots) > 0 {
		s.UpdatedAt = time.Now()
	}
	for _, event := range tx.audit {
		s.appendAuditLocked(event)
	}
	return nil
}

func (tx *Transaction) GetClient(id string) (domain.Client, bool) {
	client, ok := tx.session.Clients[id]
	return client, ok
}

func (tx *Transaction) GetOrder(id string) (domain.Order, bool) {
	if order, ok := tx.orders[id]; ok {
		return order, true
	}
	order, ok := tx.session.Orders[id]
	return order, ok
}

func (tx *Transaction) GetSlot(id string) (domain.Slot, bool) {
	if slot, ok := tx.slots[id]; ok {
		return slot, true
	}
	slot, ok := tx.session.Slots[id]
	return slot, ok
}

func (tx *Transaction) FindOrderByIdempotencyKey(key string) (domain.Order, bool) {
	if key == "" {
		return domain.Order{}, false
	}
	for _, order := range tx.orders {
		if order.IdempotencyKey == key {
			return order, true
		}
	}
	for id := range tx.session.Orders {
		order, _ := tx.GetOrder(id)
		if order.IdempotencyKey == key {
			return order, true
		}
	}
	return domain.Order{}, false
}

func (tx *Transaction) SaveOrder(order domain.Order) {
	order.SessionID = tx.session.SessionID
	tx.orders[order.ID] = order
}

func (tx *Transaction) ReserveSlot(slotID, orderID string) (domain.Slot, domain.Slot, error) {
	slot, ok := tx.GetSlot(slotID)
	if !ok {
		return domain.Slot{}, domain.Slot{}, fmt.Errorf("slot %s not found", slotID)
	}
	if !slot.IsAvailable {
		return slot, slot, fmt.Errorf("slot %s is already reserved", slotID)
	}
	before := slot
	slot.IsAvailable = false
	slot.ReservedBy = orderID
	tx.slots[slotID] = slot
	return before, slot, nil
}

func (tx *Transaction) ReleaseSlot(slotID, orderID string) (domain.Slot, domain.Slot, error) {
	slot, ok := tx.GetSlot(slotID)
	if !ok {
		return domain.Slot{}, domain.Slot{}, fmt.Errorf("slot %s not found", slotID)
	}
	if slot.ReservedBy != orderID {
		return slot, slot, fmt.Errorf("slot %s is not reserved by order %s", slotID, orderID)
	}
	before := slot
	slot.IsAvailable = true
	slot.ReservedBy = ""
	tx.slots[slotID] = slot
	return before, slot, nil
}

func (tx *Transaction) AppendAudit(event domain.AuditEvent) {
	tx.audit = append(tx.audit, event)
}
