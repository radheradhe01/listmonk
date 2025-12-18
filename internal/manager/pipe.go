package manager

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/knadh/listmonk/models"
	"github.com/paulbellamy/ratecounter"
)

// scheduledMessage holds a campaign message and the time at which it should
// be released to the manager's campMsgQ for sending.
type scheduledMessage struct {
	msg CampaignMessage
	at  time.Time
}

type pipe struct {
	camp       *models.Campaign
	rate       *ratecounter.RateCounter
	wg         *sync.WaitGroup
	sent       atomic.Int64
	lastID     atomic.Uint64
	errors     atomic.Uint64
	stopped    atomic.Bool
	withErrors atomic.Bool

	// Queue of scheduled messages for this campaign.
	schedQ chan scheduledMessage
	// Number of messages scheduled for the current UTC hour (not yet recorded as sent).
	scheduled atomic.Int64
	// UTC hour (0-23) for which `scheduled` is valid.
	scheduledHour atomic.Int64

	m *Manager
}

// newPipe adds a campaign to the process queue.
func (m *Manager) newPipe(c *models.Campaign) (*pipe, error) {
	// Validate messenger.
	if _, ok := m.messengers[c.Messenger]; !ok {
		m.store.UpdateCampaignStatus(c.ID, models.CampaignStatusCancelled)
		return nil, fmt.Errorf("unknown messenger %s on campaign %s", c.Messenger, c.Name)
	}

	// Load the template.
	if err := c.CompileTemplate(m.TemplateFuncs(c)); err != nil {
		return nil, err
	}

	// Load any media/attachments.
	if err := m.attachMedia(c); err != nil {
		return nil, err
	}

	// Add the campaign to the active map.
	p := &pipe{
		camp: c,
		rate: ratecounter.NewRateCounter(time.Minute),
		wg:   &sync.WaitGroup{},
		m:    m,
		// buffered queue to avoid blocking the DB fetcher; size tuned to batch size.
		schedQ: make(chan scheduledMessage, m.cfg.BatchSize*2),
	}

	// Increment the waitgroup so that Wait() blocks immediately. This is necessary
	// as a campaign pipe is created first and subscribers/messages under it are
	// fetched asynchronolusly later. The messages each add to the wg and that
	// count is used to determine the exhaustion/completion of all messages.
	p.wg.Add(1)

	// Start the per-pipe scheduler goroutine that releases scheduled messages
	// to the manager's queue at their scheduled times.
	go p.runScheduler()

	go func() {
		// Wait for all the messages in the campaign to be processed
		// (successfully or skipped after errors or cancellation).
		p.wg.Wait()

		p.cleanup()
	}()

	m.pipesMut.Lock()
	m.pipes[c.ID] = p
	m.pipesMut.Unlock()
	return p, nil
}

// runScheduler drains the pipe's schedQ and releases messages to the manager's
// campMsgQ at their scheduled times. It also enforces the sliding-window check
// (if configured) at the time of actual release.
func (p *pipe) runScheduler() {
	for sm := range p.schedQ {
		// Wait until the scheduled time (simple sleep).
		now := time.Now()
		if sm.at.After(now) {
			time.Sleep(sm.at.Sub(now))
		}

		// If the campaign has been stopped in the meantime, drop the message
		// and mark it done so the waitgroup can be released.
		if p.stopped.Load() {
			sm.msg.pipe.wg.Done()
			continue
		}

		// Sliding window enforcement is done here so scheduled messages respect
		// the global sliding window limit at send time.
		hasSliding := p.m.cfg.SlidingWindow &&
			p.m.cfg.SlidingWindowRate > 0 &&
			p.m.cfg.SlidingWindowDuration.Seconds() > 1

		if hasSliding {
			diff := time.Since(p.m.slidingStart)

			// Window has expired. Reset the clock.
			if diff >= p.m.cfg.SlidingWindowDuration {
				p.m.slidingStart = time.Now()
				p.m.slidingCount = 0
			}

			// Have the messages exceeded the limit?
			p.m.slidingCount++
			if p.m.slidingCount >= p.m.cfg.SlidingWindowRate {
				wait := p.m.cfg.SlidingWindowDuration - diff

				p.m.log.Printf("messages exceeded (%d) for the window (%v since %s). Sleeping for %s.",
					p.m.slidingCount,
					p.m.cfg.SlidingWindowDuration,
					p.m.slidingStart.Format(time.RFC822Z),
					wait.Round(time.Second)*1)

				p.m.slidingCount = 0
				time.Sleep(wait)
			}
		}

		// If the campaign was stopped during the wait, drop the message.
		if p.stopped.Load() {
			sm.msg.pipe.wg.Done()
			continue
		}

		// Push the message to the manager queue for workers to pick up.
		p.m.campMsgQ <- sm.msg
	}
}

// NextSubscribers processes the next batch of subscribers in a given campaign.
// It returns a bool indicating whether any subscribers were processed
// in the current batch or not. A false indicates that all subscribers
// have been processed, or that a campaign has been paused or cancelled.
func (p *pipe) NextSubscribers() (bool, error) {
	// Start with default limit.
	limit := p.m.cfg.BatchSize

	// Determine if the campaign has a per-day quota.
	hasQuota := p.camp.DailyQuota.Valid && p.camp.DailyQuota.Int > 0

	now := time.Now().UTC()

	// If there's a per-campaign daily quota, compute per-hour allowance and
	// subtract already-sent and already-scheduled messages for the current UTC hour.
	if hasQuota {
		daily := p.camp.DailyQuota.Int
		perHour := (daily + 23) / 24 // ceil(daily/24)

		// Reset scheduled counter if the hour rolled over.
		currentHour := now.Hour()
		if int(p.scheduledHour.Load()) != currentHour {
			p.scheduled.Store(0)
			p.scheduledHour.Store(int64(currentHour))
		}

		// Get already-recorded sent count for this campaign in the current hour.
		sentThisHour, err := p.m.store.GetCampaignHourlySent(p.camp.ID, now)
		if err != nil {
			return false, fmt.Errorf("error fetching campaign hourly sent count (%s): %v", p.camp.Name, err)
		}

		// Account for messages already scheduled for this hour to avoid overscheduling.
		scheduledCount := int(p.scheduled.Load())

		allowed := perHour - sentThisHour - scheduledCount
		if allowed <= 0 {
			// No quota left for this UTC hour. Schedule a retry at the start of the next hour.
			nextHour := now.Truncate(time.Hour).Add(time.Hour)
			wait := time.Until(nextHour)
			if wait < time.Second {
				wait = time.Second
			}

			// Keep the pipe alive so cleanup does not trigger. We'll release this extra
			// hold when the scheduled goroutine pushes the pipe back into the manager queue.
			p.wg.Add(1)
			go func(pr *pipe, d time.Duration) {
				// Sleep until the scheduled retry time.
				select {
				case <-time.After(d):
					// Try to requeue the pipe. Non-blocking to avoid deadlocks if queue is full.
					select {
					case pr.m.nextPipes <- pr:
					default:
					}
				}

				// Release the extra waitgroup counter so the pipe can be cleaned up normally later.
				pr.wg.Done()
			}(p, wait)

			return false, nil
		}
		if allowed < limit {
			limit = allowed
		}
	}

	// Fetch subscribers capped by computed limit.
	subs, err := p.m.store.NextSubscribers(p.camp.ID, limit)
	if err != nil {
		return false, fmt.Errorf("error fetching campaign subscribers (%s): %v", p.camp.Name, err)
	}

	if len(subs) == 0 {
		return false, nil
	}

	// If quota is configured, schedule messages evenly over the remainder of the hour.
	if hasQuota {
		now := time.Now().UTC()

		// Recompute allowance (best-effort) and spacing across the rest of the hour.
		sentThisHour, _ := p.m.store.GetCampaignHourlySent(p.camp.ID, now)
		perHour := (p.camp.DailyQuota.Int + 23) / 24
		remaining := perHour - sentThisHour - int(p.scheduled.Load())
		if remaining <= 0 {
			// guard in case DB changed between checks
			return false, nil
		}
		if remaining < len(subs) {
			remaining = len(subs)
		}

		nextHour := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, time.UTC)
		rest := nextHour.Sub(now)

		var spacing time.Duration
		if remaining > 0 {
			spacing = time.Duration(int64(rest) / int64(remaining))
		}

		for i, s := range subs {
			msg, err := p.newMessage(s)
			if err != nil {
				p.m.log.Printf("error rendering message (%s) (%s): %v", p.camp.Name, s.Email, err)
				continue
			}

			// Add a small jitter to spread exact times a bit.
			var jitter time.Duration
			if spacing > time.Second {
				jrange := int64(spacing / 10)
				if jrange > 0 {
					jitter = time.Duration(rand.Int63n(jrange) - jrange/2)
				}
			}

			scheduledAt := time.Now().Add(time.Duration(i)*spacing + jitter)

			// Enqueue scheduled message.
			p.schedQ <- scheduledMessage{msg: msg, at: scheduledAt}

			// Account for this scheduled message so subsequent scheduling doesn't overshoot.
			p.scheduled.Add(1)
		}
	} else {
		// No per-campaign quota: behave as before, pushing immediately while
		// honoring the sliding window (global) settings.
		hasSliding := p.m.cfg.SlidingWindow &&
			p.m.cfg.SlidingWindowRate > 0 &&
			p.m.cfg.SlidingWindowDuration.Seconds() > 1

		for _, s := range subs {
			msg, err := p.newMessage(s)
			if err != nil {
				p.m.log.Printf("error rendering message (%s) (%s): %v", p.camp.Name, s.Email, err)
				continue
			}

			// Push the message to the queue while blocking and waiting until
			// the queue is drained.
			p.m.campMsgQ <- msg

			// Check if the sliding window is active.
			if hasSliding {
				diff := time.Since(p.m.slidingStart)

				// Window has expired. Reset the clock.
				if diff >= p.m.cfg.SlidingWindowDuration {
					p.m.slidingStart = time.Now()
					p.m.slidingCount = 0
					continue
				}

				// Have the messages exceeded the limit?
				p.m.slidingCount++
				if p.m.slidingCount >= p.m.cfg.SlidingWindowRate {
					wait := p.m.cfg.SlidingWindowDuration - diff

					p.m.log.Printf("messages exceeded (%d) for the window (%v since %s). Sleeping for %s.",
						p.m.slidingCount,
						p.m.cfg.SlidingWindowDuration,
						p.m.slidingStart.Format(time.RFC822Z),
						wait.Round(time.Second)*1)

					p.m.slidingCount = 0
					time.Sleep(wait)
				}
			}
		}
	}

	return true, nil
}

// OnError keeps track of the number of errors that occur while sending messages
// and pauses the campaign if the error threshold is met.
func (p *pipe) OnError() {
	if p.m.cfg.MaxSendErrors < 1 {
		return
	}

	// If the error threshold is met, pause the campaign.
	count := p.errors.Add(1)
	if int(count) < p.m.cfg.MaxSendErrors {
		return
	}

	p.Stop(true)
	p.m.log.Printf("error count exceeded %d. pausing campaign %s", p.m.cfg.MaxSendErrors, p.camp.Name)
}

// Stop "marks" a campaign as stopped. It doesn't actually stop the processing
// of messages. That happens when every queued message in the campaign is processed,
// marking .wg, the waitgroup counter as done. That triggers cleanup().
func (p *pipe) Stop(withErrors bool) {
	// Already stopped.
	if p.stopped.Load() {
		return
	}

	if withErrors {
		p.withErrors.Store(true)
	}

	p.stopped.Store(true)
}

// newMessage returns a campaign message while internally incrementing the
// number of messages in the pipe wait group so that the status of every
// message can be atomically tracked.
func (p *pipe) newMessage(s models.Subscriber) (CampaignMessage, error) {
	msg, err := p.m.NewCampaignMessage(p.camp, s)
	if err != nil {
		return msg, err
	}

	msg.pipe = p
	p.wg.Add(1)

	return msg, nil
}

// cleanup finishes the campaign and updates the campaign status in the DB
// and also triggers a notification to the admin. This only triggers once
// a pipe's wg counter is fully exhausted, draining all messages in its queue.
func (p *pipe) cleanup() {
	// Close the scheduler queue so the per-pipe scheduler goroutine can exit gracefully.
	// This ensures the scheduler goroutine does not leak after the pipe is being cleaned up.
	if p.schedQ != nil {
		close(p.schedQ)
	}

	defer func() {
		p.m.pipesMut.Lock()
		delete(p.m.pipes, p.camp.ID)
		p.m.pipesMut.Unlock()
	}()

	// Update campaign's 'sent count.
	if err := p.m.store.UpdateCampaignCounts(p.camp.ID, 0, int(p.sent.Load()), int(p.lastID.Load())); err != nil {
		p.m.log.Printf("error updating campaign counts (%s): %v", p.camp.Name, err)
	}

	// The campaign was auto-paused due to errors.
	if p.withErrors.Load() {
		if err := p.m.store.UpdateCampaignStatus(p.camp.ID, models.CampaignStatusPaused); err != nil {
			p.m.log.Printf("error updating campaign (%s) status to %s: %v", p.camp.Name, models.CampaignStatusPaused, err)
		} else {
			p.m.log.Printf("set campaign (%s) to %s", p.camp.Name, models.CampaignStatusPaused)
		}

		_ = p.m.sendNotif(p.camp, models.CampaignStatusPaused, "Too many errors")
		return
	}

	// The campaign was manually stopped (pause, cancel).
	if p.stopped.Load() {
		p.m.log.Printf("stop processing campaign (%s)", p.camp.Name)
		return
	}

	// Campaign wasn't manually stopped and subscribers were naturally exhausted.
	// Fetch the up-to-date campaign status from the DB.
	c, err := p.m.store.GetCampaign(p.camp.ID)
	if err != nil {
		p.m.log.Printf("error fetching campaign (%s) for ending: %v", p.camp.Name, err)
		return
	}

	// If a running campaign has exhausted subscribers, it's finished.
	if c.Status == models.CampaignStatusRunning || c.Status == models.CampaignStatusScheduled {
		c.Status = models.CampaignStatusFinished
		if err := p.m.store.UpdateCampaignStatus(p.camp.ID, models.CampaignStatusFinished); err != nil {
			p.m.log.Printf("error finishing campaign (%s): %v", p.camp.Name, err)
		} else {
			p.m.log.Printf("campaign (%s) finished", p.camp.Name)
		}
	} else {
		p.m.log.Printf("finish processing campaign (%s)", p.camp.Name)
	}

	// Notify admin.
	_ = p.m.sendNotif(c, c.Status, "")
}
