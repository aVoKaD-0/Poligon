package store

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"poligon/environment/internal/domain"
	"poligon/environment/internal/faults"
)

// SessionState encapsulates all isolated state for a single test session/run.
type SessionState struct {
	mu           sync.RWMutex
	SessionID    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Clients      map[string]domain.Client
	Slots        map[string]domain.Slot
	Orders       map[string]domain.Order
	AuditLog     []domain.AuditEvent
	FaultEngine  *faults.Engine
	initSnapshot *domain.StateSnapshot
}

// Manager manages multiple isolated test sessions.
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*SessionState
}

// NewManager creates a new store manager.
func NewManager() *Manager {
	return &Manager{
		sessions: make(map[string]*SessionState),
	}
}

// GetOrCreate returns an existing session or creates a new one with default seed data.
func (m *Manager) GetOrCreate(sessionID string) *SessionState {
	if sessionID == "" {
		sessionID = "default"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if sess, exists := m.sessions[sessionID]; exists {
		return sess
	}

	sess := newDefaultSession(sessionID, nil)
	m.sessions[sessionID] = sess
	return sess
}

// InitSession initializes or resets a session with custom or default fixtures and fault rules.
func (m *Manager) InitSession(sessionID string, clients []domain.Client, slots []domain.Slot, faultRules []domain.FaultRule) *SessionState {
	if sessionID == "" {
		sessionID = "default"
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	sess := newSessionWithFixtures(sessionID, clients, slots, faultRules)
	m.sessions[sessionID] = sess
	return sess
}

// DeleteSession removes a session from memory.
func (m *Manager) DeleteSession(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, sessionID)
}

// ListSessionIDs returns all currently active session IDs.
func (m *Manager) ListSessionIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	return ids
}

// Reset restores a session to its initial baseline state.
func (s *SessionState) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.initSnapshot == nil {
		return
	}

	s.Clients = make(map[string]domain.Client)
	for k, v := range s.initSnapshot.Clients {
		s.Clients[k] = v
	}

	s.Slots = make(map[string]domain.Slot)
	for k, v := range s.initSnapshot.Slots {
		s.Slots[k] = v
	}

	s.Orders = make(map[string]domain.Order)
	for k, v := range s.initSnapshot.Orders {
		s.Orders[k] = v
	}

	s.UpdatedAt = time.Now()
	s.AuditLog = append(s.AuditLog, domain.AuditEvent{
		ID:             fmt.Sprintf("audit-%d", time.Now().UnixNano()),
		SessionID:      s.SessionID,
		Timestamp:      time.Now(),
		Action:         domain.AuditActionResetState,
		EntityType:     "session",
		EntityID:       s.SessionID,
		ResponseStatus: 200,
	})
}

// GetSnapshot returns a full copy of the current session state.
func (s *SessionState) GetSnapshot() domain.StateSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	clientsCopy := make(map[string]domain.Client, len(s.Clients))
	for k, v := range s.Clients {
		clientsCopy[k] = v
	}

	slotsCopy := make(map[string]domain.Slot, len(s.Slots))
	for k, v := range s.Slots {
		slotsCopy[k] = v
	}

	ordersCopy := make(map[string]domain.Order, len(s.Orders))
	activeCount := 0
	for k, v := range s.Orders {
		ordersCopy[k] = v
		if v.Status == domain.OrderStatusActive {
			activeCount++
		}
	}

	return domain.StateSnapshot{
		SessionID:         s.SessionID,
		CreatedAt:         s.CreatedAt,
		UpdatedAt:         s.UpdatedAt,
		Clients:           clientsCopy,
		Slots:             slotsCopy,
		Orders:            ordersCopy,
		ActiveOrdersCount: activeCount,
		TotalOrdersCount:  len(s.Orders),
	}
}

// GetAuditLog returns a slice of audit log events.
func (s *SessionState) GetAuditLog() []domain.AuditEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]domain.AuditEvent, len(s.AuditLog))
	copy(result, s.AuditLog)
	return result
}

// AppendAudit records a state change or significant API event.
func (s *SessionState) AppendAudit(event domain.AuditEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if event.ID == "" {
		event.ID = fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	event.SessionID = s.SessionID
	s.AuditLog = append(s.AuditLog, event)
}

// Verify evaluates the current state against expected assertions.
func (s *SessionState) Verify(criteria domain.VerificationCriteria) domain.VerificationReport {
	s.mu.RLock()
	defer s.mu.RUnlock()

	report := domain.VerificationReport{
		SessionID:   s.SessionID,
		CheckedAt:   time.Now(),
		OverallPass: true,
		Assertions:  make([]domain.AssertionResult, 0),
		TotalOrders: len(s.Orders),
	}

	activeOrders := make([]domain.Order, 0)
	ordersByClient := make(map[string][]domain.Order)
	for _, ord := range s.Orders {
		if ord.Status == domain.OrderStatusActive {
			activeOrders = append(activeOrders, ord)
		}
		ordersByClient[ord.ClientID] = append(ordersByClient[ord.ClientID], ord)
	}
	report.ActiveOrders = len(activeOrders)

	// Assertion 1: Expected active orders count
	if criteria.ExpectedActiveOrdersCount != nil {
		expected := *criteria.ExpectedActiveOrdersCount
		pass := len(activeOrders) == expected
		details := []string{
			fmt.Sprintf("Expected active orders: %d, Actual: %d", expected, len(activeOrders)),
		}
		for _, o := range activeOrders {
			details = append(details, fmt.Sprintf("Active Order: id=%s client_id=%s date=%s time=%s", o.ID, o.ClientID, o.Date, o.TimeWindow))
		}
		if !pass {
			report.OverallPass = false
		}
		report.Assertions = append(report.Assertions, domain.AssertionResult{
			Name:    "expected_active_orders_count",
			Passed:  pass,
			Message: fmt.Sprintf("Checked total active orders: %d expected, %d found", expected, len(activeOrders)),
			Details: details,
		})
	}

	// Assertion 2: Disallow duplicates for client
	if criteria.DisallowDuplicates {
		clientID := criteria.ClientID
		if clientID != "" {
			clientActiveOrders := make([]domain.Order, 0)
			for _, ord := range ordersByClient[clientID] {
				if ord.Status == domain.OrderStatusActive {
					clientActiveOrders = append(clientActiveOrders, ord)
				}
			}
			pass := len(clientActiveOrders) <= 1
			details := []string{
				fmt.Sprintf("Client %s active orders count: %d", clientID, len(clientActiveOrders)),
			}
			for _, ord := range clientActiveOrders {
				details = append(details, fmt.Sprintf("Order %s: date=%s, time=%s, address=%s", ord.ID, ord.Date, ord.TimeWindow, ord.Address))
			}
			if !pass {
				report.OverallPass = false
			}
			report.Assertions = append(report.Assertions, domain.AssertionResult{
				Name:    "no_duplicate_orders_for_client",
				Passed:  pass,
				Message: fmt.Sprintf("Client %s duplicate check (max 1 active allowed)", clientID),
				Details: details,
			})
		}
	}

	// Assertion 3: Verify address on latest order
	if criteria.ExpectedAddress != "" && len(activeOrders) > 0 {
		latest := activeOrders[len(activeOrders)-1]
		pass := strings.TrimSpace(latest.Address) == strings.TrimSpace(criteria.ExpectedAddress)
		if !pass {
			report.OverallPass = false
		}
		report.Assertions = append(report.Assertions, domain.AssertionResult{
			Name:    "expected_address_match",
			Passed:  pass,
			Message: fmt.Sprintf("Checking address matches expected: '%s'", criteria.ExpectedAddress),
			Details: []string{
				fmt.Sprintf("Actual address on order %s: '%s'", latest.ID, latest.Address),
			},
		})
	}

	// Assertion 4: Verify time window
	if criteria.ExpectedTimeWindow != "" && len(activeOrders) > 0 {
		latest := activeOrders[len(activeOrders)-1]
		pass := strings.Contains(strings.ToLower(latest.TimeWindow), strings.ToLower(criteria.ExpectedTimeWindow))
		if !pass {
			report.OverallPass = false
		}
		report.Assertions = append(report.Assertions, domain.AssertionResult{
			Name:    "expected_time_window_match",
			Passed:  pass,
			Message: fmt.Sprintf("Checking time window matches expected: '%s'", criteria.ExpectedTimeWindow),
			Details: []string{
				fmt.Sprintf("Actual time window on order %s: '%s'", latest.ID, latest.TimeWindow),
			},
		})
	}

	return report
}

// Session state query & mutation methods:

func (s *SessionState) ListClients() []domain.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]domain.Client, 0, len(s.Clients))
	for _, c := range s.Clients {
		res = append(res, c)
	}
	return res
}

func (s *SessionState) GetClient(id string) (domain.Client, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.Clients[id]
	return c, ok
}

func (s *SessionState) ListSlots(date, serviceType string, onlyAvailable bool) []domain.Slot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]domain.Slot, 0, len(s.Slots))
	for _, slot := range s.Slots {
		if date != "" && slot.Date != date {
			continue
		}
		if serviceType != "" && !strings.EqualFold(slot.ServiceType, serviceType) {
			continue
		}
		if onlyAvailable && !slot.IsAvailable {
			continue
		}
		res = append(res, slot)
	}
	return res
}

func (s *SessionState) GetSlot(id string) (domain.Slot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	slot, ok := s.Slots[id]
	return slot, ok
}

func (s *SessionState) ReserveSlot(slotID, orderID string) (before domain.Slot, after domain.Slot, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	slot, ok := s.Slots[slotID]
	if !ok {
		return domain.Slot{}, domain.Slot{}, fmt.Errorf("slot %s not found", slotID)
	}
	if !slot.IsAvailable {
		return slot, slot, fmt.Errorf("slot %s is already reserved", slotID)
	}

	before = slot
	slot.IsAvailable = false
	slot.ReservedBy = orderID
	s.Slots[slotID] = slot
	s.UpdatedAt = time.Now()
	return before, slot, nil
}

func (s *SessionState) ReleaseSlot(slotID string) (before domain.Slot, after domain.Slot, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	slot, ok := s.Slots[slotID]
	if !ok {
		return domain.Slot{}, domain.Slot{}, fmt.Errorf("slot %s not found", slotID)
	}

	before = slot
	slot.IsAvailable = true
	slot.ReservedBy = ""
	s.Slots[slotID] = slot
	s.UpdatedAt = time.Now()
	return before, slot, nil
}

func (s *SessionState) ListOrders(clientID string, status domain.OrderStatus) []domain.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()

	res := make([]domain.Order, 0, len(s.Orders))
	for _, ord := range s.Orders {
		if clientID != "" && ord.ClientID != clientID {
			continue
		}
		if status != "" && ord.Status != status {
			continue
		}
		res = append(res, ord)
	}
	return res
}

func (s *SessionState) GetOrder(id string) (domain.Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ord, ok := s.Orders[id]
	return ord, ok
}

func (s *SessionState) FindOrderByIdempotencyKey(key string) (domain.Order, bool) {
	if key == "" {
		return domain.Order{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ord := range s.Orders {
		if ord.IdempotencyKey == key {
			return ord, true
		}
	}
	return domain.Order{}, false
}

func (s *SessionState) SaveOrder(order domain.Order) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Orders[order.ID] = order
	s.UpdatedAt = time.Now()
}

func (s *SessionState) Faults() *faults.Engine {
	return s.FaultEngine
}

// Helpers for seeding:

func newDefaultSession(sessionID string, faultRules []domain.FaultRule) *SessionState {
	defaultClients := []domain.Client{
		{
			ID:        "client-001",
			Name:      "Иван Иванов",
			Phone:     "+7 (999) 111-22-33",
			Email:     "ivanov@example.com",
			Address:   "г. Москва, ул. Тверская, д. 12, кв. 34",
			Notes:     "Постоянный клиент",
			CreatedAt: time.Now().Add(-24 * time.Hour),
		},
		{
			ID:        "client-002",
			Name:      "Анна Смирнова",
			Phone:     "+7 (999) 222-33-44",
			Email:     "smirnova@example.com",
			Address:   "г. Москва, ул. Ленина, д. 5, кв. 10",
			CreatedAt: time.Now().Add(-48 * time.Hour),
		},
	}

	defaultSlots := []domain.Slot{
		{
			ID:          "slot-101",
			MasterID:    "master-1",
			MasterName:  "Алексей Петров",
			Date:        "2026-10-30",
			StartTime:   "18:00",
			EndTime:     "19:00",
			ServiceType: "выездное обслуживание",
			IsAvailable: true,
		},
		{
			ID:          "slot-102",
			MasterID:    "master-1",
			MasterName:  "Алексей Петров",
			Date:        "2026-10-30",
			StartTime:   "19:00",
			EndTime:     "20:00",
			ServiceType: "выездное обслуживание",
			IsAvailable: true,
		},
		{
			ID:          "slot-103",
			MasterID:    "master-2",
			MasterName:  "Дмитрий Кузнецов",
			Date:        "2026-10-31",
			StartTime:   "10:00",
			EndTime:     "12:00",
			ServiceType: "выездное обслуживание",
			IsAvailable: true,
		},
		{
			ID:          "slot-104",
			MasterID:    "master-2",
			MasterName:  "Дмитрий Кузнецов",
			Date:        "2026-10-31",
			StartTime:   "14:00",
			EndTime:     "16:00",
			ServiceType: "выездное обслуживание",
			IsAvailable: true,
		},
		{
			ID:          "slot-105",
			MasterID:    "master-1",
			MasterName:  "Алексей Петров",
			Date:        "2026-10-31",
			StartTime:   "18:00",
			EndTime:     "20:00",
			ServiceType: "выездное обслуживание",
			IsAvailable: true,
		},
	}

	return newSessionWithFixtures(sessionID, defaultClients, defaultSlots, faultRules)
}

func newSessionWithFixtures(sessionID string, clients []domain.Client, slots []domain.Slot, faultRules []domain.FaultRule) *SessionState {
	now := time.Now()
	clientsMap := make(map[string]domain.Client)
	for _, c := range clients {
		clientsMap[c.ID] = c
	}

	slotsMap := make(map[string]domain.Slot)
	for _, s := range slots {
		slotsMap[s.ID] = s
	}

	ordersMap := make(map[string]domain.Order)

	initSnapshot := &domain.StateSnapshot{
		SessionID:         sessionID,
		CreatedAt:         now,
		UpdatedAt:         now,
		Clients:           copyClientsMap(clientsMap),
		Slots:             copySlotsMap(slotsMap),
		Orders:            copyOrdersMap(ordersMap),
		ActiveOrdersCount: 0,
		TotalOrdersCount:  0,
	}

	sess := &SessionState{
		SessionID:    sessionID,
		CreatedAt:    now,
		UpdatedAt:    now,
		Clients:      clientsMap,
		Slots:        slotsMap,
		Orders:       ordersMap,
		AuditLog:     make([]domain.AuditEvent, 0),
		FaultEngine:  faults.NewEngine(faultRules),
		initSnapshot: initSnapshot,
	}

	return sess
}

func copyClientsMap(m map[string]domain.Client) map[string]domain.Client {
	res := make(map[string]domain.Client, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}

func copySlotsMap(m map[string]domain.Slot) map[string]domain.Slot {
	res := make(map[string]domain.Slot, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}

func copyOrdersMap(m map[string]domain.Order) map[string]domain.Order {
	res := make(map[string]domain.Order, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}
