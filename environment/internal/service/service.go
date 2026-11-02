package service

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/store"
	"poligon/environment/pkg/api"
)

var (
	ErrNotFound       = errors.New("resource not found")
	ErrConflict       = errors.New("conflict with current state")
	ErrBadRequest     = errors.New("invalid request parameters")
	ErrOrderCancelled = errors.New("order is already cancelled")
)

var orderSeq uint64 = 1000

// Service provides CRM business operations with session isolation and audit trail.
type Service struct {
	manager *store.Manager
}

// NewService creates a new business service.
func NewService(m *store.Manager) *Service {
	return &Service{manager: m}
}

// Public request aliases preserve compatibility with existing internal callers.
type CreateOrderRequest = api.CreateOrderRequest
type UpdateOrderRequest = api.UpdateOrderRequest

// ListClients returns all clients in the session.
func (s *Service) ListClients(sessionID string) []domain.Client {
	sess := s.manager.GetOrCreate(sessionID)
	return sess.ListClients()
}

// GetClient returns a single client.
func (s *Service) GetClient(sessionID, clientID string) (domain.Client, error) {
	sess := s.manager.GetOrCreate(sessionID)
	client, ok := sess.GetClient(clientID)
	if !ok {
		return domain.Client{}, ErrNotFound
	}
	return client, nil
}

// ListSlots returns available appointment slots matching the filter.
func (s *Service) ListSlots(sessionID, date, serviceType string, onlyAvailable bool) []domain.Slot {
	sess := s.manager.GetOrCreate(sessionID)
	return sess.ListSlots(date, serviceType, onlyAvailable)
}

// ListOrders returns all orders in the session.
func (s *Service) ListOrders(sessionID, clientID string, status domain.OrderStatus) []domain.Order {
	sess := s.manager.GetOrCreate(sessionID)
	return sess.ListOrders(clientID, status)
}

// GetOrder returns an order by ID.
func (s *Service) GetOrder(sessionID, orderID string) (domain.Order, error) {
	sess := s.manager.GetOrCreate(sessionID)
	ord, ok := sess.GetOrder(orderID)
	if !ok {
		return domain.Order{}, ErrNotFound
	}
	return ord, nil
}

// CreateOrder handles order creation, slot reservation, and idempotency tracking.
func (s *Service) CreateOrder(sessionID string, req CreateOrderRequest) (order domain.Order, idempotent bool, err error) {
	sess := s.manager.GetOrCreate(sessionID)
	err = sess.Transact(func(tx *store.Transaction) error {
		order, idempotent, err = createOrder(tx, sess.SessionID, req)
		return err
	})
	return order, idempotent, err
}

func createOrder(tx *store.Transaction, sessionID string, req CreateOrderRequest) (domain.Order, bool, error) {
	// Validate required fields
	if req.ClientID == "" {
		return domain.Order{}, false, fmt.Errorf("%w: client_id is required", ErrBadRequest)
	}
	if req.Address == "" {
		return domain.Order{}, false, fmt.Errorf("%w: address is required", ErrBadRequest)
	}
	if req.Date == "" && req.SlotID == "" {
		return domain.Order{}, false, fmt.Errorf("%w: date or slot_id is required", ErrBadRequest)
	}

	// Idempotency check: if an order with this idempotency key already exists, return it!
	if req.IdempotencyKey != "" {
		if existing, found := tx.FindOrderByIdempotencyKey(req.IdempotencyKey); found {
			tx.AppendAudit(domain.AuditEvent{
				Action:         domain.AuditActionCreateOrder,
				EntityType:     "order",
				EntityID:       existing.ID,
				Method:         "POST",
				Path:           "/api/v1/orders",
				IdempotencyKey: req.IdempotencyKey,
				RequestPayload: req,
				ResponseStatus: 200,
				AfterState:     existing,
				ErrorMessage:   "Idempotent duplicate request recognized - returned existing order",
			})
			return existing, true, nil
		}
	}

	client, clientFound := tx.GetClient(req.ClientID)
	var clientName, clientPhone string
	if clientFound {
		clientName = client.Name
		clientPhone = client.Phone
	}

	var reservedSlot domain.Slot
	if req.SlotID != "" {
		slot, ok := tx.GetSlot(req.SlotID)
		if !ok {
			return domain.Order{}, false, fmt.Errorf("%w: slot %s not found", ErrNotFound, req.SlotID)
		}
		if !slot.IsAvailable {
			return domain.Order{}, false, fmt.Errorf("%w: slot %s is already reserved", ErrConflict, req.SlotID)
		}
		reservedSlot = slot
		if err := applySlot(&req.Date, &req.TimeWindow, slot); err != nil {
			return domain.Order{}, false, err
		}
		if req.ServiceType != "" && req.ServiceType != slot.ServiceType {
			return domain.Order{}, false, fmt.Errorf("%w: service_type does not match slot", ErrBadRequest)
		}
		req.ServiceType = slot.ServiceType
	}

	seq := atomic.AddUint64(&orderSeq, 1)
	orderID := fmt.Sprintf("ord-%d", seq)
	now := time.Now()

	serviceType := req.ServiceType
	if serviceType == "" {
		serviceType = "выездное обслуживание"
	}

	order := domain.Order{
		ID:             orderID,
		SessionID:      sessionID,
		ClientID:       req.ClientID,
		ClientName:     clientName,
		ClientPhone:    clientPhone,
		MasterID:       reservedSlot.MasterID,
		SlotID:         req.SlotID,
		ServiceType:    serviceType,
		Address:        req.Address,
		Date:           req.Date,
		TimeWindow:     req.TimeWindow,
		Status:         domain.OrderStatusActive,
		Notes:          req.Notes,
		IdempotencyKey: req.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	if req.SlotID != "" {
		beforeSlot, afterSlot, err := tx.ReserveSlot(req.SlotID, orderID)
		if err != nil {
			return domain.Order{}, false, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		tx.AppendAudit(domain.AuditEvent{
			Action:         domain.AuditActionReserveSlot,
			EntityType:     "slot",
			EntityID:       req.SlotID,
			Method:         "POST",
			Path:           "/api/v1/orders",
			ResponseStatus: 201,
			BeforeState:    beforeSlot,
			AfterState:     afterSlot,
		})
	}

	tx.SaveOrder(order)

	tx.AppendAudit(domain.AuditEvent{
		Action:         domain.AuditActionCreateOrder,
		EntityType:     "order",
		EntityID:       order.ID,
		Method:         "POST",
		Path:           "/api/v1/orders",
		IdempotencyKey: req.IdempotencyKey,
		RequestPayload: req,
		ResponseStatus: 201,
		AfterState:     order,
	})

	return order, false, nil
}

// UpdateOrder updates details, address, or slot for an active order.
func (s *Service) UpdateOrder(sessionID, orderID string, req UpdateOrderRequest) (order domain.Order, err error) {
	sess := s.manager.GetOrCreate(sessionID)
	err = sess.Transact(func(tx *store.Transaction) error {
		order, err = updateOrder(tx, orderID, req)
		return err
	})
	return order, err
}

func updateOrder(tx *store.Transaction, orderID string, req UpdateOrderRequest) (domain.Order, error) {
	order, ok := tx.GetOrder(orderID)
	if !ok {
		return domain.Order{}, ErrNotFound
	}
	if order.Status == domain.OrderStatusCancelled {
		return domain.Order{}, fmt.Errorf("%w: cannot update cancelled order", ErrConflict)
	}

	beforeState := order

	if req.Address != nil && *req.Address != "" {
		order.Address = *req.Address
	}
	if req.Date != nil && *req.Date != "" {
		order.Date = *req.Date
	}
	if req.TimeWindow != nil && *req.TimeWindow != "" {
		order.TimeWindow = *req.TimeWindow
	}
	if req.Notes != nil {
		order.Notes = *req.Notes
	}

	newSlotID := order.SlotID
	if req.SlotID != nil && *req.SlotID != "" {
		newSlotID = *req.SlotID
	}
	if newSlotID != "" {
		slot, exists := tx.GetSlot(newSlotID)
		if !exists {
			return domain.Order{}, fmt.Errorf("%w: slot %s not found", ErrNotFound, newSlotID)
		}
		if newSlotID != order.SlotID {
			// Omitted date/time refer to the newly selected slot, not the old one.
			order.Date, order.TimeWindow = "", ""
			if req.Date != nil {
				order.Date = *req.Date
			}
			if req.TimeWindow != nil {
				order.TimeWindow = *req.TimeWindow
			}
		}
		if err := applySlot(&order.Date, &order.TimeWindow, slot); err != nil {
			return domain.Order{}, err
		}
		if newSlotID != order.SlotID {
			bSlot, aSlot, err := tx.ReserveSlot(newSlotID, order.ID)
			if err != nil {
				return domain.Order{}, fmt.Errorf("%w: %v", ErrConflict, err)
			}
			tx.AppendAudit(domain.AuditEvent{
				Action: domain.AuditActionReserveSlot, EntityType: "slot", EntityID: newSlotID,
				BeforeState: bSlot, AfterState: aSlot,
			})
			if order.SlotID != "" {
				bSlot, aSlot, err := tx.ReleaseSlot(order.SlotID, order.ID)
				if err != nil {
					return domain.Order{}, fmt.Errorf("%w: %v", ErrConflict, err)
				}
				tx.AppendAudit(domain.AuditEvent{
					Action: domain.AuditActionReleaseSlot, EntityType: "slot", EntityID: order.SlotID,
					BeforeState: bSlot, AfterState: aSlot,
				})
			}
			order.SlotID = newSlotID
			order.MasterID = slot.MasterID
			order.ServiceType = slot.ServiceType
		}
	}

	order.UpdatedAt = time.Now()
	tx.SaveOrder(order)

	tx.AppendAudit(domain.AuditEvent{
		Action:         domain.AuditActionUpdateOrder,
		EntityType:     "order",
		EntityID:       order.ID,
		Method:         "PATCH",
		Path:           fmt.Sprintf("/api/v1/orders/%s", orderID),
		RequestPayload: req,
		ResponseStatus: 200,
		BeforeState:    beforeState,
		AfterState:     order,
	})

	return order, nil
}

// CancelOrder cancels an active order and frees any associated slot.
func (s *Service) CancelOrder(sessionID, orderID string) (order domain.Order, err error) {
	sess := s.manager.GetOrCreate(sessionID)
	err = sess.Transact(func(tx *store.Transaction) error {
		order, err = cancelOrder(tx, orderID)
		return err
	})
	return order, err
}

func cancelOrder(tx *store.Transaction, orderID string) (domain.Order, error) {
	order, ok := tx.GetOrder(orderID)
	if !ok {
		return domain.Order{}, ErrNotFound
	}
	if order.Status == domain.OrderStatusCancelled {
		return order, nil // Idempotent cancel
	}

	beforeState := order
	order.Status = domain.OrderStatusCancelled
	order.UpdatedAt = time.Now()

	// Release slot
	if order.SlotID != "" {
		bSlot, aSlot, err := tx.ReleaseSlot(order.SlotID, order.ID)
		if err != nil {
			return domain.Order{}, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		tx.AppendAudit(domain.AuditEvent{
			Action:      domain.AuditActionReleaseSlot,
			EntityType:  "slot",
			EntityID:    order.SlotID,
			BeforeState: bSlot,
			AfterState:  aSlot,
		})
	}

	tx.SaveOrder(order)

	tx.AppendAudit(domain.AuditEvent{
		Action:         domain.AuditActionCancelOrder,
		EntityType:     "order",
		EntityID:       order.ID,
		Method:         "POST",
		Path:           fmt.Sprintf("/api/v1/orders/%s/cancel", orderID),
		ResponseStatus: 200,
		BeforeState:    beforeState,
		AfterState:     order,
	})

	return order, nil
}

// applySlot rejects contradictory booking parameters and fills omitted values.
func applySlot(date, window *string, slot domain.Slot) error {
	expectedWindow := fmt.Sprintf("%s-%s", slot.StartTime, slot.EndTime)
	if *date != "" && *date != slot.Date {
		return fmt.Errorf("%w: date does not match slot %s", ErrBadRequest, slot.ID)
	}
	if *window != "" && *window != expectedWindow {
		return fmt.Errorf("%w: time_window does not match slot %s", ErrBadRequest, slot.ID)
	}
	*date, *window = slot.Date, expectedWindow
	return nil
}
