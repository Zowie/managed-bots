package gcalbot

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/keybase/managed-bots/base"

	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func (h *HTTPSrv) handleEventUpdateWebhook(w http.ResponseWriter, r *http.Request) {
	var err error
	defer func() {
		if err != nil {
			h.Errorf("error in event update webhook: %s", err)
		}
	}()

	state := r.Header.Get("X-Goog-Resource-State")
	if state == "sync" {
		// sync header, safe to ignore
		return
	}

	channelID := r.Header.Get("X-Goog-Channel-ID")
	resourceID := r.Header.Get("X-Goog-Resource-ID")
	channel, err := h.db.GetChannelByChannelID(channelID)
	if err != nil {
		return
	} else if channel == nil {
		h.Debug("channel not found: %s", channelID)
		return
	}

	// sanity check
	if channel.ResourceID != resourceID {
		err = fmt.Errorf("channel and request resourceIDs do not match: %s != %s",
			channel.ResourceID, resourceID)
		return
	}

	token, err := h.db.GetToken(channel.AccountID)
	if err != nil {
		return
	}

	reminderSubscriptions, err := h.db.GetAggregatedSubscriptionsByTypeForUserAndCal(channel.AccountID, channel.CalendarID, SubscriptionTypeReminder)
	if err != nil {
		return
	}
	inviteSubscriptions, err := h.db.GetAggregatedSubscriptionsByTypeForUserAndCal(channel.AccountID, channel.CalendarID, SubscriptionTypeInvite)
	if err != nil {
		return
	}

	client := h.handler.config.Client(context.Background(), token)
	srv, err := calendar.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return
	}

	registerForReminders := func(start time.Time, isAllDay bool, event *calendar.Event) {
		if isAllDay {
			// TODO(marcel): support all day event reminders
			return
		}
		// check if the event starts in the next 3 hours before registering it
		if time.Now().Before(start) && time.Now().Add(3*time.Hour).After(start) {
			for _, subscription := range reminderSubscriptions {
				err = h.reminderScheduler.UpdateOrCreateReminderEvent(srv, event, subscription)
				if err != nil {
					return
				}
			}
		}
	}

	sendInvites := func(end time.Time, event *calendar.Event) {
		if event.RecurringEventId != "" && event.RecurringEventId != event.Id {
			// if the event is recurring, only deal with the underlying recurring event
			return
		}
		if time.Now().After(end) {
			// the event has already ended, don't send an invite
			return
		}
		var exists bool
		exists, err = h.db.ExistsInvite(channel.AccountID, channel.CalendarID, event.Id)
		if err != nil {
			return
		}
		if !exists {
			// user was recently invited to the event
			for range inviteSubscriptions {
				// TODO(marcel): use subscription convid
				err = h.handler.sendEventInvite(srv, channel, event)
				if err != nil {
					return
				}
			}
		}
	}

	var events []*calendar.Event
	nextSyncToken := channel.NextSyncToken
	err = srv.Events.
		List(channel.CalendarID).
		SyncToken(channel.NextSyncToken).
		Pages(context.Background(), func(page *calendar.Events) error {
			nextSyncToken = page.NextSyncToken
			events = append(events, page.Items...)
			return nil
		})
	switch typedErr := err.(type) {
	case nil:
	case *googleapi.Error:
		if typedErr.Code == 410 {
			// TODO(marcel): next sync token has expired, need to do a "full refresh"
			// could lead to really old events not in db having invites sent out
			return
		}
	default:
		err = fmt.Errorf("error updating events for account ID '%s', cal '%s': %s",
			channel.AccountID, channel.CalendarID, typedErr)
		return
	}

	for _, event := range events {
		status := EventStatus(event.Status)

		if status == EventStatusCancelled {
			for _, subscription := range reminderSubscriptions {
				err = h.reminderScheduler.UpdateOrCreateReminderEvent(srv, event, subscription)
				if err != nil {
					return
				}
			}
			continue
		}

		var start, end time.Time
		var isAllDay bool
		start, end, isAllDay, err = ParseTime(event.Start, event.End)
		if err != nil {
			return
		}

		if event.Attendees == nil {
			// the event has no attendees, the user created it! register for reminders
			registerForReminders(start, isAllDay, event)
		}

		for _, attendee := range event.Attendees {
			responseStatus := ResponseStatus(attendee.ResponseStatus)
			if attendee.Self && (responseStatus == ResponseStatusAccepted || responseStatus == ResponseStatusTentative) {
				// the user has (possibly tentatively) accepted the event invite, register for reminders
				registerForReminders(start, isAllDay, event)
			} else if attendee.Self && !attendee.Organizer && responseStatus == ResponseStatusNeedsAction &&
				status != EventStatusCancelled {
				// the user has not responded to the event invite, send event invites
				sendInvites(end, event)
			}
		}
	}

	err = h.db.UpdateChannelNextSyncToken(channelID, nextSyncToken)
	if err != nil {
		return
	}

	w.WriteHeader(200)
}

func (h *Handler) createSubscription(
	srv *calendar.Service, subscription Subscription,
) (exists bool, err error) {
	exists, err = h.db.ExistsSubscription(subscription)
	if err != nil || exists {
		// if no error, subscription exists, short circuit
		return exists, err
	}

	err = h.createEventChannel(srv, subscription.AccountID, subscription.CalendarID)
	if err != nil {
		return exists, err
	}

	err = h.db.InsertSubscription(subscription)
	if err != nil {
		return exists, err
	}

	h.reminderScheduler.AddSubscription(subscription)

	return false, nil
}

func (h *Handler) removeSubscription(
	srv *calendar.Service, subscription Subscription,
) (exists bool, err error) {
	exists, err = h.db.DeleteSubscription(subscription)
	if err != nil || !exists {
		// if no error, subscription doesn't exist, short circuit
		return exists, err
	}

	h.reminderScheduler.RemoveSubscription(subscription)

	subscriptionCount, err := h.db.CountSubscriptionsByAccountAndCalID(subscription.AccountID, subscription.CalendarID)
	if err != nil {
		return exists, err
	}

	if subscriptionCount == 0 {
		// if there are no more subscriptions for this account + calendar, remove the channel
		channel, err := h.db.GetChannelByAccountAndCalendarID(subscription.AccountID, subscription.CalendarID)
		if err != nil {
			return exists, err
		}

		if channel != nil {
			err = srv.Channels.Stop(&calendar.Channel{
				Id:         channel.ChannelID,
				ResourceId: channel.ResourceID,
			}).Do()
			switch err := err.(type) {
			case nil:
			case *googleapi.Error:
				if err.Code != 404 {
					return exists, err
				}
				// if the channel wasn't found, don't return
			default:
				return exists, err
			}

			err = h.db.DeleteChannelByChannelID(channel.ChannelID)
			if err != nil {
				return exists, err
			}
		}
	}

	return true, nil
}

func (h *Handler) createEventChannel(
	srv *calendar.Service,
	accountID, calendarID string,
) error {
	exists, err := h.db.ExistsChannelByAccountAndCalID(accountID, calendarID)
	if err != nil || exists {
		// if err is nil but the channel exists, return
		return err
	}

	// channel not found, create one
	channelID, err := base.MakeRequestID()
	if err != nil {
		return err
	}

	// TODO(marcel): possibly fill in existing invites into db
	// get all events simply to get the NextSyncToken and begin receiving invites from there
	// request no fields so that the responses are tiny and fast
	var nextSyncToken string
	err = srv.Events.List(calendarID).Fields().Pages(context.Background(), func(page *calendar.Events) error {
		if page.NextPageToken == "" {
			nextSyncToken = page.NextSyncToken
		}
		return nil
	})
	if err != nil {
		return err
	}

	// open channel
	res, err := srv.Events.Watch(calendarID, &calendar.Channel{
		Address: fmt.Sprintf("%s/gcalbot/events/webhook", h.httpPrefix),
		Id:      channelID,
		Type:    "web_hook",
	}).Do()
	if err != nil {
		return err
	}

	err = h.db.InsertChannel(Channel{
		ChannelID:     channelID,
		AccountID:     accountID,
		CalendarID:    calendarID,
		ResourceID:    res.ResourceId,
		Expiry:        time.Unix(res.Expiration/1e3, 0),
		NextSyncToken: nextSyncToken,
	})

	return err
}

type RenewChannelScheduler struct {
	*base.DebugOutput
	sync.Mutex

	shutdownCh chan struct{}

	db         *DB
	config     *oauth2.Config
	httpPrefix string
}

func NewRenewChannelScheduler(
	debugConfig *base.ChatDebugOutputConfig,
	db *DB,
	config *oauth2.Config,
	httpPrefix string,
) *RenewChannelScheduler {
	return &RenewChannelScheduler{
		DebugOutput: base.NewDebugOutput("RenewChannelScheduler", debugConfig),
		db:          db,
		config:      config,
		httpPrefix:  httpPrefix,
		shutdownCh:  make(chan struct{}),
	}
}

func (r *RenewChannelScheduler) Shutdown() error {
	r.Lock()
	defer r.Unlock()
	if r.shutdownCh != nil {
		close(r.shutdownCh)
		r.shutdownCh = nil
	}
	return nil
}

func (r *RenewChannelScheduler) Run() error {
	r.Lock()
	shutdownCh := r.shutdownCh
	r.Unlock()
	r.renewScheduler(shutdownCh)
	return nil
}

func (r *RenewChannelScheduler) renewScheduler(shutdownCh chan struct{}) {
	ticker := time.NewTicker(time.Hour)
	defer func() {
		ticker.Stop()
		r.Debug("shutting down")
	}()
	for {
		select {
		case <-shutdownCh:
			return
		case <-ticker.C:
			channels, err := r.db.GetExpiringChannelList()
			if err != nil {
				r.Errorf("error getting expiring channels: %s", err)
			}
			for _, channel := range channels {
				select {
				case <-shutdownCh:
					return
				default:
				}
				err = r.renewChannel(channel)
				if err != nil {
					r.Errorf("error renewing channel '%s': %s", channel.ChannelID, err)
				}
			}
		}
	}
}

func (r *RenewChannelScheduler) renewChannel(channel *Channel) error {
	token, err := r.db.GetToken(channel.AccountID)
	if err != nil {
		return err
	}

	client := r.config.Client(context.Background(), token)
	srv, err := calendar.NewService(context.Background(), option.WithHTTPClient(client))
	if err != nil {
		return err
	}

	newChannelID, err := base.MakeRequestID()
	if err != nil {
		return err
	}

	// open new channel
	res, err := srv.Events.Watch(channel.CalendarID, &calendar.Channel{
		Address: fmt.Sprintf("%s/gcalbot/events/webhook", r.httpPrefix),
		Id:      newChannelID,
		Type:    "web_hook",
	}).Do()
	if err != nil {
		return err
	}

	err = r.db.UpdateChannel(channel.ChannelID, newChannelID, time.Unix(res.Expiration/1e3, 0))
	if err != nil {
		return err
	}

	// close old channel
	err = srv.Channels.Stop(&calendar.Channel{
		Id:         channel.ChannelID,
		ResourceId: channel.ResourceID,
	}).Do()
	switch err := err.(type) {
	case nil:
	case *googleapi.Error:
		if err.Code != 404 {
			return err
		}
		// if the channel wasn't found, don't return an error
	default:
		return err
	}

	return nil
}
