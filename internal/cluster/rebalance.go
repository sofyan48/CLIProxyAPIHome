package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	rebalanceCapability = "cpa_rebalance_v1"
	rebalanceInterval   = 30 * time.Second
	rebalancePlanTTL    = 90 * time.Second
	rebalanceCooldown   = 3 * time.Minute
	rebalancePending    = "pending"
	rebalanceExecuting  = "executing"
	rebalanceCompleted  = "completed"
	rebalanceFailed     = "failed"
	rebalanceCanceled   = "canceled"
	rebalanceMaxActive  = 4
)

type CPARebalanceClientRecord struct {
	Fingerprint string    `gorm:"column:fingerprint;primaryKey"`
	ConnectedAt time.Time `gorm:"column:connected_at;not null"`
	// NextEligibleAt gates all destinations, including failure backoff and success cooldown.
	NextEligibleAt time.Time `gorm:"column:next_eligible_at"`
	// FailedTarget and TargetRetryAt describe the latest failure, not independent target gates.
	FailedTarget   string        `gorm:"column:failed_target"`
	TargetRetryAt  time.Time     `gorm:"column:target_retry_at"`
	FailureBackoff time.Duration `gorm:"column:failure_backoff;not null;default:0"`
	UpdatedAt      time.Time     `gorm:"column:updated_at"`
}

func (CPARebalanceClientRecord) TableName() string { return "cpa_rebalance_clients" }

type CPARebalancePlanRecord struct {
	ID                string    `gorm:"column:id;primaryKey"`
	Term              int64     `gorm:"column:term;not null"`
	Fingerprint       string    `gorm:"column:fingerprint;not null;index"`
	SourceConnectedAt time.Time `gorm:"column:source_connected_at;not null"`
	SourceIP          string    `gorm:"column:source_ip;not null"`
	SourcePort        int       `gorm:"column:source_port;not null"`
	SourceStartedAt   time.Time `gorm:"column:source_started_at;not null"`
	TargetIP          string    `gorm:"column:target_ip;not null"`
	TargetPort        int       `gorm:"column:target_port;not null"`
	TargetStartedAt   time.Time `gorm:"column:target_started_at;not null"`
	State             string    `gorm:"column:state;not null;index:idx_rebalance_active,priority:1"`
	ExpiresAt         time.Time `gorm:"column:expires_at;not null;index:idx_rebalance_active,priority:2"`
	CreatedAt         time.Time `gorm:"column:created_at;not null"`
	UpdatedAt         time.Time `gorm:"column:updated_at;not null;index"`
}

func (CPARebalancePlanRecord) TableName() string { return "cpa_rebalance_plans" }

type RebalanceInstruction struct {
	ID     string          `json:"id"`
	Term   int64           `json:"term"`
	Target RebalanceTarget `json:"target"`
}

type RebalanceTarget struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type rebalanceHome struct {
	id      HomeIncarnationID
	capable bool
}

func rebalanceHomeKey(home HomeIncarnationID) string {
	return fmt.Sprintf("%s/%d", net.JoinHostPort(home.IP, strconv.Itoa(home.Port)), home.StartedAt.UnixNano())
}

// lockRebalanceGate serializes planning, claims, results, and election changes.
func lockRebalanceGate(tx *gorm.DB) (ClusterMasterGateRecord, time.Time, error) {
	gate := ClusterMasterGateRecord{ID: 1}
	if errCreate := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&gate).Error; errCreate != nil {
		return gate, time.Time{}, errCreate
	}
	if errLock := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&gate, "id = ?", 1).Error; errLock != nil {
		return gate, time.Time{}, errLock
	}
	now, errNow := DatabaseNow(tx.Statement.Context, tx)
	return gate, now, errNow
}

func rebalanceLeaderValid(tx *gorm.DB, gate ClusterMasterGateRecord, now time.Time, timeout time.Duration) (bool, error) {
	if gate.Term <= 0 || !now.Before(gate.LeaseUntil) {
		return false, nil
	}
	var masters []ClusterNodeRecord
	if errFind := tx.Where("is_master = ? AND last_seen_at >= ?", true, now.Add(-timeout)).Find(&masters).Error; errFind != nil {
		return false, errFind
	}
	if len(masters) != 1 || gate.MasterKey != rebalanceHomeKey(HomeIncarnationID{IP: masters[0].IP, Port: masters[0].Port, StartedAt: masters[0].StartedAt}) {
		return false, nil
	}
	var active int64
	errCount := tx.Model(&HomeProcessIncarnationRecord{}).Where("home_ip = ? AND home_port = ? AND started_at = ? AND state = ? AND last_seen_at >= ?", masters[0].IP, masters[0].Port, masters[0].StartedAt, HomeIncarnationActive, now.Add(-timeout)).Count(&active).Error
	return active == 1, errCount
}

func loadRebalanceHomes(tx *gorm.DB, now time.Time, timeout time.Duration) (map[string]rebalanceHome, error) {
	var nodes []ClusterNodeRecord
	if errFind := tx.Where("last_seen_at >= ?", now.Add(-timeout)).Find(&nodes).Error; errFind != nil {
		return nil, errFind
	}
	var processes []HomeProcessIncarnationRecord
	if errFind := tx.Where("state = ? AND last_seen_at >= ?", HomeIncarnationActive, now.Add(-timeout)).Find(&processes).Error; errFind != nil {
		return nil, errFind
	}
	processByKey := make(map[string]HomeProcessIncarnationRecord, len(processes))
	for _, process := range processes {
		processByKey[rebalanceHomeKey(HomeIncarnationID{IP: process.HomeIP, Port: process.HomePort, StartedAt: process.StartedAt})] = process
	}
	homes := make(map[string]rebalanceHome, len(nodes))
	for _, node := range nodes {
		id := HomeIncarnationID{IP: node.IP, Port: node.Port, StartedAt: node.StartedAt}
		key := rebalanceHomeKey(id)
		process, ok := processByKey[key]
		if !ok {
			continue
		}
		var capabilities []string
		if errJSON := json.Unmarshal(process.Capabilities, &capabilities); errJSON != nil {
			return nil, errJSON
		}
		homes[key] = rebalanceHome{id: id, capable: slices.Contains(capabilities, rebalanceCapability)}
	}
	return homes, nil
}

func loadRebalanceMembers(tx *gorm.DB, homes map[string]rebalanceHome, now time.Time) ([]CPANodeMembershipRecord, map[string]int, error) {
	var members []CPANodeMembershipRecord
	if errFind := tx.Where("state = ?", MembershipStateActive).Order("certificate_fingerprint").Find(&members).Error; errFind != nil {
		return nil, nil, errFind
	}
	loads := make(map[string]int, len(homes))
	for key := range homes {
		loads[key] = 0
	}
	live := members[:0]
	for _, member := range members {
		key := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
		if _, exists := homes[key]; !exists || member.CPAHeartbeatTimeout <= 0 || !now.Before(member.LastSeenAt.Add(member.CPAHeartbeatTimeout)) {
			continue
		}
		loads[key]++
		live = append(live, member)
	}
	return live, loads, nil
}

func rebalanceSource(plan CPARebalancePlanRecord) HomeIncarnationID {
	return HomeIncarnationID{IP: plan.SourceIP, Port: plan.SourcePort, StartedAt: plan.SourceStartedAt}
}

func rebalanceTarget(plan CPARebalancePlanRecord) HomeIncarnationID {
	return HomeIncarnationID{IP: plan.TargetIP, Port: plan.TargetPort, StartedAt: plan.TargetStartedAt}
}

// reserveRebalanceLoad projects an authorized move without counting a disconnected CPA twice.
// The return value adds an executing CPA missing from the live membership total.
func reserveRebalanceLoad(plan CPARebalancePlanRecord, members map[string]CPANodeMembershipRecord, loads map[string]int) int {
	member, live := members[plan.Fingerprint]
	if plan.State != rebalanceExecuting && (!live || !member.ConnectedAt.Equal(plan.SourceConnectedAt) || !membershipOwnedByHome(member, rebalanceSource(plan))) {
		return 0
	}
	targetKey := rebalanceHomeKey(rebalanceTarget(plan))
	if _, targetLive := loads[targetKey]; !targetLive {
		// Keep the execution slot, but do not invent a live Home for load calculations.
		return 0
	}
	loads[targetKey]++
	if live {
		currentKey := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
		loads[currentKey]--
		return 0
	}
	return 1
}

func finishRebalancePlan(tx *gorm.DB, plan *CPARebalancePlanRecord, state string, now time.Time) error {
	if plan.State != rebalancePending && plan.State != rebalanceExecuting {
		return nil
	}
	plan.State, plan.UpdatedAt = state, now
	if errSave := tx.Save(plan).Error; errSave != nil {
		return errSave
	}
	var client CPARebalanceClientRecord
	if errFirst := tx.First(&client, "fingerprint = ?", plan.Fingerprint).Error; errFirst != nil {
		return errFirst
	}
	nextEligibleAt := now.Add(rebalanceInterval)
	if state == rebalanceCompleted {
		client.NextEligibleAt = now.Add(rebalanceCooldown)
		client.FailureBackoff = 0
		client.FailedTarget = ""
		client.TargetRetryAt = time.Time{}
	} else if state == rebalanceFailed {
		client.FailureBackoff = min(max(time.Minute, client.FailureBackoff*2), 10*time.Minute)
		client.FailedTarget = rebalanceHomeKey(rebalanceTarget(*plan))
		client.TargetRetryAt = now.Add(client.FailureBackoff)
		nextEligibleAt = client.TargetRetryAt
	}
	// Changing targets or canceling another plan must not shorten an outstanding backoff.
	if state != rebalanceCompleted && client.NextEligibleAt.Before(nextEligibleAt) {
		client.NextEligibleAt = nextEligibleAt
	}
	client.UpdatedAt = now
	return tx.Save(&client).Error
}

// ReconcileRebalances is the only planner. The persisted election gate fences stale masters.
func (r *Repository) ReconcileRebalances(ctx context.Context, home HomeIncarnationID, timeout time.Duration) error {
	db, errDB := r.database()
	if errDB != nil {
		return errDB
	}
	return db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		gate, now, errGate := lockRebalanceGate(tx)
		if errGate != nil {
			return errGate
		}
		valid, errLeader := rebalanceLeaderValid(tx, gate, now, timeout)
		if errLeader != nil {
			return errLeader
		}
		if !valid || gate.MasterKey != rebalanceHomeKey(home) {
			return nil
		}
		homes, errHomes := loadRebalanceHomes(tx, now, timeout)
		if errHomes != nil {
			return errHomes
		}
		if _, ok := homes[rebalanceHomeKey(home)]; !ok {
			return nil
		}
		members, loads, errMembers := loadRebalanceMembers(tx, homes, now)
		if errMembers != nil {
			return errMembers
		}
		byFingerprint := make(map[string]CPANodeMembershipRecord, len(members))
		total := len(members)
		for _, member := range members {
			byFingerprint[member.CertificateFingerprint] = member
		}
		var plans []CPARebalancePlanRecord
		if errFind := tx.Where("state IN ?", []string{rebalancePending, rebalanceExecuting}).Find(&plans).Error; errFind != nil {
			return errFind
		}
		busy := make(map[string]bool)
		active := 0
		for index := range plans {
			plan := &plans[index]
			member, exists := byFingerprint[plan.Fingerprint]
			sourceMatches := exists && membershipOwnedByHome(member, rebalanceSource(*plan)) && member.ConnectedAt.Equal(plan.SourceConnectedAt)
			targetMatches := exists && membershipOwnedByHome(member, rebalanceTarget(*plan)) && member.ConnectedAt.After(plan.SourceConnectedAt)
			_, targetLive := homes[rebalanceHomeKey(rebalanceTarget(*plan))]
			state := ""
			// Source liveness can lapse while the CPA settles pending dispatches.
			// Executing plans keep capacity until completion, explicit failure, or expiry.
			switch {
			case plan.State == rebalanceExecuting && targetMatches:
				state = rebalanceCompleted
			case !now.Before(plan.ExpiresAt):
				state = rebalanceFailed
			case plan.State == rebalancePending && (!sourceMatches || plan.Term != gate.Term):
				state = rebalanceCanceled
			case plan.State == rebalancePending && !targetLive:
				state = rebalanceFailed
			}
			if state != "" {
				if errFinish := finishRebalancePlan(tx, plan, state, now); errFinish != nil {
					return errFinish
				}
				continue
			}
			busy[plan.Fingerprint] = true
			active++
			total += reserveRebalanceLoad(*plan, byFingerprint, loads)
		}
		if errDelete := tx.Where("state NOT IN ? AND updated_at < ?", []string{rebalancePending, rebalanceExecuting}, now.Add(-24*time.Hour)).Delete(&CPARebalancePlanRecord{}).Error; errDelete != nil {
			return errDelete
		}
		if len(homes) < 2 || now.Sub(gate.LastBalanceAt) < rebalanceInterval {
			return nil
		}
		gate.LastBalanceAt = now
		if errSave := tx.Save(&gate).Error; errSave != nil {
			return errSave
		}
		var clients []CPARebalanceClientRecord
		if errFind := tx.Find(&clients).Error; errFind != nil {
			return errFind
		}
		capabilities := make(map[string]CPARebalanceClientRecord, len(clients))
		for _, client := range clients {
			capabilities[client.Fingerprint] = client
		}
		average := float64(total) / float64(len(homes))
		quotas := make(map[string]int, len(homes))
		for key, count := range loads {
			if float64(count) > average {
				quotas[key] = max(1, int(math.Min(float64(count)*0.25, (float64(count)-average)*0.5)))
			}
		}
		// Reserve both ends immediately so concurrent plans cannot all pick the same empty Home.
		for active < rebalanceMaxActive {
			var chosen CPANodeMembershipRecord
			sourceKey, targetKey := "", ""
			for _, member := range members {
				client, supported := capabilities[member.CertificateFingerprint]
				if !supported || busy[member.CertificateFingerprint] || !client.ConnectedAt.Equal(member.ConnectedAt) || now.Before(client.NextEligibleAt) || member.ProtocolVersion != 1 {
					continue
				}
				if _, live := byFingerprint[member.CertificateFingerprint]; !live {
					continue
				}
				key := rebalanceHomeKey(HomeIncarnationID{IP: member.HomeIP, Port: member.HomePort, StartedAt: member.HomeStartedAt})
				if quotas[key] <= 0 || !homes[key].capable || float64(loads[key]) <= average {
					continue
				}
				for destination, candidate := range homes {
					if !candidate.capable || loads[key]-loads[destination] < 2 || float64(loads[destination]) >= average {
						continue
					}
					if sourceKey == "" || loads[key] > loads[sourceKey] || (loads[key] == loads[sourceKey] && (loads[destination] < loads[targetKey] || (loads[destination] == loads[targetKey] && destination < targetKey))) {
						chosen, sourceKey, targetKey = member, key, destination
					}
				}
			}
			if sourceKey == "" {
				break
			}
			target := homes[targetKey].id
			plan := CPARebalancePlanRecord{ID: uuid.NewString(), Term: gate.Term, Fingerprint: chosen.CertificateFingerprint,
				SourceConnectedAt: chosen.ConnectedAt, SourceIP: chosen.HomeIP, SourcePort: chosen.HomePort, SourceStartedAt: chosen.HomeStartedAt,
				TargetIP: target.IP, TargetPort: target.Port, TargetStartedAt: target.StartedAt,
				State: rebalancePending, ExpiresAt: now.Add(rebalancePlanTTL), CreatedAt: now, UpdatedAt: now}
			if errCreate := tx.Create(&plan).Error; errCreate != nil {
				return errCreate
			}
			busy[chosen.CertificateFingerprint] = true
			quotas[sourceKey]--
			loads[sourceKey]--
			loads[targetKey]++
			active++
		}
		return nil
	})
}

func (c *Coordinator) runRebalanceLoop(ctx context.Context) {
	ticker := time.NewTicker(rebalanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !c.IsMaster() {
				continue
			}
			home, initialized := c.HomeIncarnation()
			if !initialized {
				continue
			}
			if errReconcile := c.repo.ReconcileRebalances(ctx, home, c.heartbeatTimeout); errReconcile != nil && ctx.Err() == nil {
				log.Warnf("failed to reconcile Home rebalance plans: %v", errReconcile)
			}
		}
	}
}

// RebalanceInstruction reads candidate plans without locking the election gate.
// CLAIM revalidates leadership and membership under the gate before authorizing a move.
func (h *RESPHandler) RebalanceInstruction(ctx context.Context, lifetime ConnectionLifetime) ([]byte, error) {
	if h == nil || h.repo == nil || h.repo.db == nil || h.coordinator == nil {
		return nil, fmt.Errorf("cluster rebalance unavailable")
	}
	db := h.repo.db.WithContext(contextOrBackground(ctx))
	var gate ClusterMasterGateRecord
	if errGate := db.First(&gate, "id = ?", 1).Error; errGate != nil {
		return nil, errGate
	}
	now, errNow := DatabaseNow(ctx, db)
	if errNow != nil {
		return nil, errNow
	}
	valid, errLeader := rebalanceLeaderValid(db, gate, now, h.coordinator.heartbeatTimeout)
	if errLeader != nil || !valid {
		return nil, errLeader
	}
	var plan CPARebalancePlanRecord
	errFirst := db.Where("fingerprint = ? AND source_connected_at = ? AND source_ip = ? AND source_port = ? AND source_started_at = ? AND state = ? AND term = ? AND expires_at > ?", lifetime.Fingerprint, lifetime.ConnectedAt, lifetime.Home.IP, lifetime.Home.Port, lifetime.Home.StartedAt, rebalancePending, gate.Term, now).First(&plan).Error
	if errors.Is(errFirst, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if errFirst != nil {
		return nil, errFirst
	}
	return json.Marshal(RebalanceInstruction{ID: plan.ID, Term: plan.Term, Target: RebalanceTarget{IP: plan.TargetIP, Port: plan.TargetPort}})
}

// HandleRebalance uses the mTLS-classified lifetime, never a caller-supplied CPA identity.
func (h *RESPHandler) HandleRebalance(ctx context.Context, lifetime ConnectionLifetime, args []string) ([]byte, error) {
	if h == nil || h.repo == nil || h.repo.db == nil || h.coordinator == nil {
		return nil, fmt.Errorf("cluster rebalance unavailable")
	}
	if len(args) < 3 {
		return nil, fmt.Errorf("cluster rebalance: missing action")
	}
	accepted := false
	errTransaction := h.repo.db.WithContext(contextOrBackground(ctx)).Transaction(func(tx *gorm.DB) error {
		// Local dispatch contention must not hold the cluster-wide election gate.
		if errLifetime := h.repo.LockActiveConcurrencyLifetimeTx(ctx, tx, lifetime); errLifetime != nil {
			return errLifetime
		}
		// Read database time only after both potentially blocking lock acquisitions.
		gate, now, errGate := lockRebalanceGate(tx)
		if errGate != nil {
			return errGate
		}
		switch strings.ToUpper(args[2]) {
		case "ENABLE":
			if len(args) != 3 {
				return fmt.Errorf("cluster rebalance: invalid enable arguments")
			}
			var member CPANodeMembershipRecord
			if errFirst := tx.First(&member, "certificate_fingerprint = ?", lifetime.Fingerprint).Error; errFirst != nil {
				return errFirst
			}
			if member.ProtocolVersion != 1 {
				return ErrConcurrencyProtocolRequired
			}
			client := CPARebalanceClientRecord{Fingerprint: lifetime.Fingerprint, ConnectedAt: lifetime.ConnectedAt, UpdatedAt: now}
			if errCreate := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "fingerprint"}}, DoUpdates: clause.Assignments(map[string]any{"connected_at": lifetime.ConnectedAt, "updated_at": now})}).Create(&client).Error; errCreate != nil {
				return errCreate
			}
			accepted = true
			return nil
		case "CLAIM":
			if len(args) != 7 {
				return fmt.Errorf("cluster rebalance: invalid claim arguments")
			}
			term, errTerm := strconv.ParseInt(args[4], 10, 64)
			if errTerm != nil {
				return errTerm
			}
			valid, errLeader := rebalanceLeaderValid(tx, gate, now, h.coordinator.heartbeatTimeout)
			if errLeader != nil || !valid {
				return errLeader
			}
			var plan CPARebalancePlanRecord
			errFirst := tx.First(&plan, "id = ? AND fingerprint = ?", args[3], lifetime.Fingerprint).Error
			if errors.Is(errFirst, gorm.ErrRecordNotFound) {
				return nil
			}
			if errFirst != nil {
				return errFirst
			}
			if plan.State != rebalancePending || term != gate.Term || plan.Term != term || !now.Before(plan.ExpiresAt) || !lifetime.ConnectedAt.Equal(plan.SourceConnectedAt) || rebalanceHomeKey(lifetime.Home) != rebalanceHomeKey(rebalanceSource(plan)) || plan.TargetIP != args[5] || strconv.Itoa(plan.TargetPort) != args[6] {
				return nil
			}
			homes, errHomes := loadRebalanceHomes(tx, now, h.coordinator.heartbeatTimeout)
			if errHomes != nil {
				return errHomes
			}
			if target, live := homes[rebalanceHomeKey(rebalanceTarget(plan))]; !live || !target.capable {
				return nil
			}
			members, loads, errMembers := loadRebalanceMembers(tx, homes, now)
			if errMembers != nil {
				return errMembers
			}
			current := make(map[string]CPANodeMembershipRecord, len(members))
			for _, member := range members {
				current[member.CertificateFingerprint] = member
			}
			if _, healthy := current[lifetime.Fingerprint]; !healthy {
				return finishRebalancePlan(tx, &plan, rebalanceCanceled, now)
			}
			var others []CPARebalancePlanRecord
			if errFind := tx.Where("id <> ? AND expires_at > ? AND (state = ? OR (state = ? AND term = ?))", plan.ID, now, rebalanceExecuting, rebalancePending, gate.Term).Find(&others).Error; errFind != nil {
				return errFind
			}
			for _, other := range others {
				reserveRebalanceLoad(other, current, loads)
			}
			if loads[rebalanceHomeKey(rebalanceSource(plan))]-loads[rebalanceHomeKey(rebalanceTarget(plan))] < 2 {
				return finishRebalancePlan(tx, &plan, rebalanceCanceled, now)
			}
			plan.State, plan.UpdatedAt, plan.ExpiresAt = rebalanceExecuting, now, now.Add(rebalancePlanTTL)
			if errSave := tx.Save(&plan).Error; errSave != nil {
				return errSave
			}
			accepted = true
			return nil
		case "RESULT":
			if len(args) != 5 || (args[4] != "success" && args[4] != "failed") {
				return fmt.Errorf("cluster rebalance: invalid result arguments")
			}
			var plan CPARebalancePlanRecord
			errFirst := tx.First(&plan, "id = ? AND fingerprint = ?", args[3], lifetime.Fingerprint).Error
			if errors.Is(errFirst, gorm.ErrRecordNotFound) {
				accepted = true
				return nil
			}
			if errFirst != nil {
				return errFirst
			}
			if plan.State != rebalancePending && plan.State != rebalanceExecuting {
				accepted = true
				return nil
			}
			state := rebalanceFailed
			if args[4] == "success" {
				if plan.State != rebalanceExecuting || rebalanceHomeKey(lifetime.Home) != rebalanceHomeKey(rebalanceTarget(plan)) || !lifetime.ConnectedAt.After(plan.SourceConnectedAt) {
					return nil
				}
				state = rebalanceCompleted
			}
			if errFinish := finishRebalancePlan(tx, &plan, state, now); errFinish != nil {
				return errFinish
			}
			accepted = true
			return nil
		default:
			return fmt.Errorf("cluster rebalance: unsupported action")
		}
	})
	if errTransaction != nil {
		return nil, errTransaction
	}
	return json.Marshal(map[string]any{"ok": accepted})
}
