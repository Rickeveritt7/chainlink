package fluxmonitorv2

import (
	"time"
)

// TickerManagerConfig defines time manager config
type TickerManagerConfig struct {
	PollTickerPeriod   time.Duration
	PollTickerDisabled bool
	IdleTimerPeriod    time.Duration
	IdleTimerDisabled  bool
}

// TickerManager manages the tickers
// type TickerManager struct {
// 	cfg           TickerManagerConfig
// 	isHibernating bool

// 	pollTicker       utils.PausableTicker
// 	hibernationTimer utils.ResettableTimer
// 	idleTimer        utils.ResettableTimer
// 	roundTimer       utils.ResettableTimer

// 	logFields []interface{}
// }

// // NewTickerManager constructs a new TickerManager
// func NewTickerManager(
// 	cfg TickerManagerConfig,
// 	logFields []interface{},
// ) *TickerManager {
// 	lf := append(logFields, []interface{}{
// 		"pollFrequency", cfg.PollTickerPeriod,
// 		"idleDuration", cfg.IdleTimerPeriod,
// 	})

// 	return &TickerManager{
// 		cfg:              cfg,
// 		isHibernating:    false,
// 		pollTicker:       utils.NewPausableTicker(cfg.PollTickerPeriod),
// 		hibernationTimer: utils.NewResettableTimer(),
// 		idleTimer:        utils.NewResettableTimer(),
// 		roundTimer:       utils.NewResettableTimer(),
// 		logFields:        lf,
// 	}
// }

// // Stop stops all the tickers
// func (m *TickerManager) Stop() {
// 	m.pollTicker.Destroy()
// 	m.hibernationTimer.Stop()
// 	m.idleTimer.Stop()
// 	m.roundTimer.Stop()
// }

// // PollTickerDisabled returns whether the poll ticker is enabled
// func (m *TickerManager) PollTickerDisabled() bool {
// 	return m.cfg.PollTickerDisabled
// }

// // PollTickerInterval returns the poll ticker interval
// func (m *TickerManager) PollTickerInterval() time.Duration {
// 	return m.cfg.PollTickerPeriod
// }

// // IdleTimerDisabled returns whether the idle timer is enabled
// func (m *TickerManager) IdleTimerDisabled() bool {
// 	return m.cfg.IdleTimerDisabled
// }

// // IdleTimerPeriod returns the idle timer period
// func (m *TickerManager) IdleTimerPeriod() time.Duration {
// 	return m.cfg.IdleTimerPeriod
// }

// // IsHibernating returns whether the poll ticker is hibernating
// func (m *TickerManager) IsHibernating() bool {
// 	return m.isHibernating
// }

// // PollTicks returns a channel which delivers ticks at intervals
// func (m *TickerManager) PollTicks() <-chan time.Time {
// 	return m.pollTicker.Ticks()
// }

// // HibernationTicks ticks after the timer expires
// func (m *TickerManager) HibernationTicks() <-chan time.Time {
// 	return m.hibernationTimer.Ticks()
// }

// // IdleTicks ticks after the timer expires
// func (m *TickerManager) IdleTicks() <-chan time.Time {
// 	return m.idleTimer.Ticks()
// }

// // RoundTicks ticks after the timer expires
// func (m *TickerManager) RoundTicks() <-chan time.Time {
// 	return m.roundTimer.Ticks()
// }

// // SetIsHibernating sets the hibernation status
// func (m *TickerManager) SetIsHibernating(isHibernating bool) {
// 	m.isHibernating = isHibernating
// }

// // Hibernate stops the all the tickers and enables the hibernation timer
// func (m *TickerManager) Hibernate() {
// 	m.isHibernating = true

// 	m.Reset(0, 0)
// }

// // Wake starts all tickers and disables the hibernation timer
// func (m *TickerManager) Wake(roundStartedAt, roundTimesOutAt uint64) {
// 	m.isHibernating = false

// 	m.Reset(roundStartedAt, roundTimesOutAt)
// }

// // Reset resets all the tickers.
// func (m *TickerManager) Reset(roundStartedAt, roundTimesOutAt uint64) {
// 	m.resetPollTicker()
// 	m.resetHibernationTimer()
// 	m.ResetIdleTimer(roundStartedAt)
// 	m.resetRoundTimer(roundTimesOutAt)
// }

// func (m *TickerManager) resetPollTicker() {
// 	if !m.cfg.PollTickerDisabled && !m.isHibernating {
// 		m.pollTicker.Resume()
// 	} else {
// 		m.pollTicker.Pause()
// 	}
// }

// func (m *TickerManager) resetHibernationTimer() {
// 	if !m.isHibernating {
// 		m.hibernationTimer.Stop()
// 	} else {
// 		m.hibernationTimer.Reset(hibernationPollPeriod)
// 	}
// }

// func (m *TickerManager) resetRoundTimer(roundTimesOutAt uint64) {
// 	if m.isHibernating {
// 		m.roundTimer.Stop()

// 		return
// 	}

// 	loggerFields := m.loggerFields("timesOutAt", roundTimesOutAt)

// 	if roundTimesOutAt == 0 {
// 		m.roundTimer.Stop()

// 		logger.Debugw("disabling roundTimer, no active round", loggerFields...)

// 		return
// 	}

// 	timesOutAt := time.Unix(int64(roundTimesOutAt), 0)
// 	timeUntilTimeout := time.Until(timesOutAt)

// 	if timeUntilTimeout <= 0 {
// 		m.roundTimer.Stop()

// 		logger.Debugw("roundTimer has run down; disabling", loggerFields...)
// 	} else {
// 		m.roundTimer.Reset(timeUntilTimeout)

// 		loggerFields = append(loggerFields, "value", roundTimesOutAt)
// 		logger.Debugw("updating roundState.TimesOutAt", loggerFields...)
// 	}
// }

// // ResetIdleTimer resets the idle timer
// func (m *TickerManager) ResetIdleTimer(roundStartedAtUTC uint64) {
// 	if m.isHibernating || m.cfg.IdleTimerDisabled {
// 		m.idleTimer.Stop()

// 		return
// 	} else if roundStartedAtUTC == 0 {
// 		// There is no active round, so keep using the idleTimer we already have
// 		return
// 	}

// 	startedAt := time.Unix(int64(roundStartedAtUTC), 0)
// 	idleDeadline := startedAt.Add(m.cfg.IdleTimerPeriod)
// 	timeUntilIdleDeadline := time.Until(idleDeadline)
// 	loggerFields := m.loggerFields(
// 		"startedAt", roundStartedAtUTC,
// 		"timeUntilIdleDeadline", timeUntilIdleDeadline,
// 	)

// 	if timeUntilIdleDeadline <= 0 {
// 		logger.Debugw("not resetting idleTimer, negative duration", loggerFields...)

// 		return
// 	}

// 	m.idleTimer.Reset(timeUntilIdleDeadline)

// 	logger.Debugw("resetting idleTimer", loggerFields...)
// }

// func (m *TickerManager) loggerFields(added ...interface{}) []interface{} {
// 	return append(m.logFields, added...)
// }
