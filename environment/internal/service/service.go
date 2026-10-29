package service

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/store"
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

// CreateOrderRequest is the payload to book an appointment.
type CreateOrderRequest struct {
	ClientID       string `json:"client_id"`
	ServiceType    string `json:"service_type"`
	Address        string `json:"address"`
	Date           string `json:"date"`
	TimeWindow     string `json:"time_window"`
	SlotID         string `json:"slot_id,omitempty"`
	Notes          string `json:"notes,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// UpdateOrderRequest is the payload to reschedule or update address/details.
type UpdateOrderRequest struct {
	Address    *string `json:"address,omitempty"`
	Date       *string `json:"date,omitempty"`
	TimeWindow *string `json:"time_window,omitempty"`
	SlotID     *string `json:"slot_id,omitempty"`
	Notes      *string `json:"notes,omitempty"`
}

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
func (s *Service) CreateOrder(sessionID string, req CreateOrderRequest) (domain.Order, bool, error) {
	sess := s.manager.GetOrCreate(sessionID)

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
		if existing, found := sess.FindOrderByIdempotencyKey(req.IdempotencyKey); found {
			sess.AppendAudit(domain.AuditEvent{
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

	client, clientFound := sess.GetClient(req.ClientID)
	var clientName, clientPhone string
	if clientFound {
		clientName = client.Name
		clientPhone = client.Phone
	}

	var reservedSlot domain.Slot
	if req.SlotID != "" {
		slot, ok := sess.GetSlot(req.SlotID)
		if !ok {
			return domain.Order{}, false, fmt.Errorf("%w: slot %s not found", ErrNotFound, req.SlotID)
		}
		if !slot.IsAvailable {
			return domain.Order{}, false, fmt.Errorf("%w: slot %s is already reserved", ErrConflict, req.SlotID)
		}
		reservedSlot = slot
		if req.Date == "" {
			req.Date = slot.Date
		}
		if req.TimeWindow == "" {
			req.TimeWindow = fmt.Sprintf("%s-%s", slot.StartTime, slot.EndTime)
		}
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
		beforeSlot, afterSlot, err := sess.ReserveSlot(req.SlotID, orderID)
		if err == nil {
			sess.AppendAudit(domain.AuditEvent{
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
	}

	sess.SaveOrder(order)

	sess.AppendAudit(domain.AuditEvent{
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
func (s *Service) UpdateOrder(sessionID, orderID string, req UpdateOrderRequest) (domain.Order, error) {
	sess := s.manager.GetOrCreate(sessionID)
	order, ok := sess.GetOrder(orderID)
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

	if req.SlotID != nil && *req.SlotID != "" && *req.SlotID != order.SlotID {
		newSlotID := *req.SlotID
		slot, exists := sess.GetSlot(newSlotID)
		if !exists {
			return domain.Order{}, fmt.Errorf("%w: slot %s not found", ErrNotFound, newSlotID)
		}
		if !slot.IsAvailable {
			return domain.Order{}, fmt.Errorf("%w: slot %s is already reserved", ErrConflict, newSlotID)
		}

		// Release old slot
		if order.SlotID != "" {
			bSlot, aSlot, _ := sess.ReleaseSlot(order.SlotID)
			sess.AppendAudit(domain.AuditEvent{
				Action:      domain.AuditActionReleaseSlot,
				EntityType:  "slot",
				EntityID:    order.SlotID,
				BeforeState: bSlot,
				AfterState:  aSlot,
			})
		}

		// Reserve new slot
		bSlot, aSlot, _ := sess.ReserveSlot(newSlotID, order.ID)
		sess.AppendAudit(domain.AuditEvent{
			Action:      domain.AuditActionReserveSlot,
			EntityType:  "slot",
			EntityID:    newSlotID,
			BeforeState: bSlot,
			AfterState:  aSlot,
		})

		order.SlotID = newSlotID
		order.MasterID = slot.MasterID
		if req.Date == nil || *req.Date == "" {
			order.Date = slot.Date
		}
		if req.TimeWindow == nil || *req.TimeWindow == "" {
			order.TimeWindow = fmt.Sprintf("%s-%s", slot.StartTime, slot.EndTime)
		}
	}

	order.UpdatedAt = time.Now()
	sess.SaveOrder(order)

	sess.AppendAudit(domain.AuditEvent{
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
func (s *Service) CancelOrder(sessionID, orderID string) (domain.Order, error) {
	sess := s.manager.GetOrCreate(sessionID)
	order, ok := sess.GetOrder(orderID)
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
		bSlot, aSlot, _ := sess.ReleaseSlot(order.SlotID)
		sess.AppendAudit(domain.AuditEvent{
			Action:      domain.AuditActionReleaseSlot,
			EntityType:  "slot",
			EntityID:    order.SlotID,
			BeforeState: bSlot,
			AfterState:  aSlot,
		})
	}

	sess.SaveOrder(order)

	sess.AppendAudit(domain.AuditEvent{
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
