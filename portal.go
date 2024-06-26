package main

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	"github.com/emersion/go-message/mail"

	"github.com/rs/zerolog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"imap-bridge/database"
)

func (br *IMAPBridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	portal, ok := br.portalsByMXID[mxid]
	if !ok {
		dbPortal, err := br.DB.Portal.GetByMXID(context.TODO(), mxid)
		if err != nil {
			br.ZLog.Err(err).Msg("Failed to get portal from database")
			return nil
		}
		return br.loadPortal(context.TODO(), dbPortal, nil)
	}

	return portal
}

type msgconvContextKey int

const (
	msgconvContextKeyIntent msgconvContextKey = iota
	msgconvContextKeyClient
)

type portalEmailMessage struct {
	message *mail.Part
	user    *User
}

type portalMatrixMessage struct {
	evt  *event.Event
	user *User
}

// Portal implementation
type Portal struct {
	*database.Portal

	bridge *IMAPBridge
	log    zerolog.Logger

	roomCreateLock sync.Mutex
	encryptLock    sync.Mutex

	emailMessages  chan portalEmailMessage
	matrixMessages chan portalMatrixMessage

	relayUser *User
}

func (portal *Portal) IsEncrypted() bool {
	return portal.Encrypted
}

func (portal *Portal) IsPrivateChat() bool {
	// FIXME
	return false
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	dmPuppet := portal.GetDMPuppet()
	if dmPuppet != nil {
		return dmPuppet.DefaultIntent()
	}

	return portal.bridge.Bot
}

func (portal *Portal) GetDMPuppet() *Puppet {
	if portal.EmailAddress == "" {
		return nil
	}
	return portal.bridge.GetPuppetByEmailAddress(portal.EmailAddress)
}

func (portal *Portal) MarkEncrypted() {
	portal.Encrypted = true
	err := portal.Update(context.TODO())
	if err != nil {
		portal.log.Err(err).Msg("Failed to update portal in database after marking as encrypted")
	}
}

func (portal *Portal) ReceiveMatrixEvent(user bridge.User, evt *event.Event) {
	if user.GetPermissionLevel() >= bridgeconfig.PermissionLevelUser || portal.HasRelaybot() {
		portal.matrixMessages <- portalMatrixMessage{user: user.(*User), evt: evt}
	}
}

func (br *IMAPBridge) GetAllIPortals() (iportals []bridge.Portal) {
	portals, err := br.dbPortalsToPortals(br.DB.Portal.GetAllWithMXID(context.TODO()))
	if err != nil {
		br.ZLog.Err(err).Msg("Failed to get all portals with mxid")
		return nil
	}
	iportals = make([]bridge.Portal, len(portals))
	for i, portal := range portals {
		iportals[i] = portal
	}
	return iportals
}

func (portal *Portal) UpdateBridgeInfo(ctx context.Context) {
	if len(portal.MXID) == 0 {
		portal.log.Debug().Msg("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debug().Msg("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(ctx, portal.MXID, event.StateBridge, stateKey, content)
	if err != nil {
		portal.log.Warn().Err(err).Msg("Failed to update m.bridge")
	}
	// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
	_, err = portal.MainIntent().SendStateEvent(ctx, portal.MXID, event.StateHalfShotBridge, stateKey, content)
	if err != nil {
		portal.log.Warn().Err(err).Msg("Failed to update uk.half-shot.bridge")
	}
}

func (portal *Portal) HasRelaybot() bool {
	return portal.bridge.Config.Bridge.Relay.Enabled && len(portal.RelayUserID) > 0
}

func (portal *Portal) getBridgeInfo() (string, string) {
	return "", ""
}

func (portal *Portal) messageLoop() {
	for {
		select {
		case msg := <-portal.matrixMessages:
			portal.handleMatrixMessages(msg)
		case msg := <-portal.emailMessages:
			portal.handleEmailMessage(msg)
		}
	}
}

func (portal *Portal) handleMatrixMessages(msg portalMatrixMessage) {
	log := portal.log.With().
		Str("action", "handle matrix event").
		Stringer("event_id", msg.evt.ID).
		Str("event_type", msg.evt.Type.String()).
		Logger()
	ctx := log.WithContext(context.TODO())

	switch msg.evt.Type {
	case event.EventMessage, event.EventSticker:
		portal.handleMatrixMessage(ctx, msg.user, msg.evt)
	default:
		log.Warn().Str("type", msg.evt.Type.Type).Msg("Unhandled matrix message type")
	}
}

func (portal *Portal) handleMatrixMessage(ctx context.Context, sender *User, evt *event.Event) {
	log := zerolog.Ctx(ctx)
	evtTS := time.UnixMilli(evt.Timestamp)
	timings := messageTimings{
		initReceive:  evt.Mautrix.ReceivedAt.Sub(evtTS),
		decrypt:      evt.Mautrix.DecryptionDuration,
		totalReceive: time.Since(evtTS),
	}
	implicitRRStart := time.Now()
	timings.implicitRR = time.Since(implicitRRStart)
	start := time.Now()

	messageAge := timings.totalReceive
	ms := metricSender{portal: portal, timings: &timings, ctx: ctx}
	log.Debug().
		Stringer("sender", evt.Sender).
		Dur("age", messageAge).
		Msg("Received message")

	errorAfter := portal.bridge.Config.Bridge.MessageHandlingTimeout.ErrorAfter
	deadline := portal.bridge.Config.Bridge.MessageHandlingTimeout.Deadline
	isScheduled, _ := evt.Content.Raw["com.beeper.scheduled"].(bool)
	if isScheduled {
		log.Debug().Msg("Message is a scheduled message, extending handling timeouts")
		errorAfter *= 10
		deadline *= 10
	}

	if errorAfter > 0 {
		remainingTime := errorAfter - messageAge
		if remainingTime < 0 {
			go ms.sendMessageMetrics(evt, errTimeoutBeforeHandling, "Timeout handling", true)
			return
		} else if remainingTime < 1*time.Second {
			log.Warn().
				Dur("remaining_time", remainingTime).
				Dur("max_timeout", errorAfter).
				Msg("Message was delayed before reaching the bridge")
		}
		go func() {
			time.Sleep(remainingTime)
			ms.sendMessageMetrics(evt, errMessageTakingLong, "Timeout handling", false)
		}()
	}

	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	timings.preproc = time.Since(start)
	start = time.Now()

	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		log.Error().Type("content_type", content).Msg("Unexpected parsed content type")
		go ms.sendMessageMetrics(evt, fmt.Errorf("%w %T", errUnexpectedParsedContentType, evt.Content.Parsed), "Error converting", true)
		return
	}

	if !sender.IsLoggedIn() {
		sender = portal.GetRelayUser()
		if sender == nil {
			go ms.sendMessageMetrics(evt, errUserNotLoggedIn, "Ignoring", true)
			return
		} else if !sender.IsLoggedIn() {
			go ms.sendMessageMetrics(evt, errRelaybotNotLoggedIn, "Ignoring", true)
			return
		}
	}

	var editTargetMsg *database.Message
	if editTarget := content.RelatesTo.GetReplaceID(); editTarget != "" {
		var err error
		editTargetMsg, err = portal.bridge.DB.Message.GetByMXID(ctx, editTarget)
		if err != nil {
			log.Err(err).Stringer("edit_target_mxid", editTarget).Msg("Failed to get edit target message")
			go ms.sendMessageMetrics(evt, errFailedToGetEditTarget, "Error converting", true)
			return
		} else if editTargetMsg == nil {
			log.Err(err).Stringer("edit_target_mxid", editTarget).Msg("Edit target message not found")
			go ms.sendMessageMetrics(evt, errEditUnknownTarget, "Error converting", true)
			return
		} else if editTargetMsg.Sender != sender.EmailAddress {
			go ms.sendMessageMetrics(evt, errEditDifferentSender, "Error converting", true)
			return
		}
		if content.NewContent != nil {
			content = content.NewContent
			evt.Content.Parsed = content
		}
	}

	// relaybotFormatted := isRelay && portal.addRelaybotFormat(ctx, realSenderMXID, evt, content)
	if content.MsgType == event.MsgNotice && !portal.bridge.Config.Bridge.BridgeNotices {
		go ms.sendMessageMetrics(evt, errMNoticeDisabled, "Error converting", true)
		return
	}
	ctx = context.WithValue(ctx, msgconvContextKeyClient, sender.Client)

	timings.convert = time.Since(start)
	start = time.Now()

	err := portal.sendEmailMessage(ctx, content.Body, sender, evt.ID)
	if err != nil {
		log.Err(err).Str("content_body", content.Body).Msg("Failed to send email")
	}

	timings.totalSend = time.Since(start)
	go ms.sendMessageMetrics(evt, err, "Error sending", true)

	var timeStamp time.Time
	timeStamp = time.Now()

	if editTargetMsg != nil {
		err = editTargetMsg.SetTimestamp(ctx, uint64(timeStamp.Unix()))
		if err != nil {
			log.Err(err).Msg("Failed to update message timestamp in database after editing")
		}
	} else {
		portal.storeMessageInDB(ctx, evt.ID, sender.EmailAddress, uint64(timeStamp.Unix()), 0)
	}
}

// FIXME: delete this
type DataMessage string

func (portal *Portal) handleEmailMessage(portalMessage portalEmailMessage) {
	sender_address := portalMessage.message.Header.Get("From")

	log := portal.log.With().
		Str("action", "handle email message").
		Str("email_address", sender_address).
		Logger()

	ctx := log.WithContext(context.Background())

	if portal.MXID == "" {
		portal.log.Debug().
			Str("email_address", sender_address).
			Msg("Creating Matrix room from incoming message")
		if err := portal.CreateMatrixRoom(ctx, portalMessage.user, sender_address); err != nil {
			portal.log.Err(err).Msg("Failed to create portal room")
			return
		}
	}

	sender := portal.bridge.GetPuppetByEmailAddress(sender_address)

	// intent := sender.IntentFor(portal)

	body, err := ioutil.ReadAll(portalMessage.message.Body)
	if err != nil {
		log.Err(err).Msg("Failed to parse email message")
		return
	}

	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    string(body),
	}

	resp, err := portal.sendMatrixEvent(ctx, portal.MainIntent(), event.EventMessage, content, nil, 0)
	if err != nil {
		log.Err(err).Msg("Failed to send message to Matrix")
		return
	}

	portal.storeMessageInDB(ctx, resp.EventID, sender.EmailAddress, uint64(time.Now().UnixMilli()), 0)
}

func (portal *Portal) sendMainIntentMessage(ctx context.Context, content *event.MessageEventContent) (*mautrix.RespSendEvent, error) {
	return portal.sendMatrixEvent(ctx, portal.MainIntent(), event.EventMessage, content, nil, 0)
}

func (portal *Portal) encrypt(ctx context.Context, intent *appservice.IntentAPI, content *event.Content, eventType event.Type) (event.Type, error) {
	if !portal.Encrypted || portal.bridge.Crypto == nil {
		return eventType, nil
	}
	intent.AddDoublePuppetValue(content)
	portal.encryptLock.Lock()
	defer portal.encryptLock.Unlock()
	err := portal.bridge.Crypto.Encrypt(ctx, portal.MXID, eventType, content)
	if err != nil {
		return eventType, fmt.Errorf("failed to encrypt event: %w", err)
	}
	return event.EventEncrypted, nil
}

func (portal *Portal) sendMatrixEvent(ctx context.Context, intent *appservice.IntentAPI, eventType event.Type, content any, extraContent map[string]any, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := event.Content{Parsed: content, Raw: extraContent}
	if eventType != event.EventReaction {
		var err error
		eventType, err = portal.encrypt(ctx, intent, &wrappedContent, eventType)
		if err != nil {
			return nil, err
		}
	}

	_, _ = intent.UserTyping(ctx, portal.MXID, false, 0)
	return intent.SendMassagedMessageEvent(ctx, portal.MXID, eventType, &wrappedContent, timestamp)
}

func (portal *Portal) getEncryptionEventContent() (evt *event.EncryptionEventContent) {
	evt = &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}
	if rot := portal.bridge.Config.Bridge.Encryption.Rotation; rot.EnableCustom {
		evt.RotationPeriodMillis = rot.Milliseconds
		evt.RotationPeriodMessages = rot.Messages
	}
	return
}

func (portal *Portal) getBridgeInfoStateKey() string {
	return fmt.Sprintf("net.maunium.imap-bridge://bridge/%v", portal.ThreadID)
}

func (portal *Portal) GetRelayUser() *User {
	if !portal.HasRelaybot() {
		return nil
	} else if portal.relayUser == nil {
		portal.relayUser = portal.bridge.GetUserByMXID(portal.RelayUserID)
	}
	return portal.relayUser
}

func (portal *Portal) addRelaybotFormat(ctx context.Context, userID id.UserID, evt *event.Event, content *event.MessageEventContent) bool {
	member := portal.MainIntent().Member(ctx, portal.MXID, userID)
	if member == nil {
		member = &event.MemberEventContent{}
	}
	// Stickers can't have captions, so force them into images when relaying
	if evt.Type == event.EventSticker {
		content.MsgType = event.MsgImage
		evt.Type = event.EventMessage
	}
	content.EnsureHasHTML()
	data, err := portal.bridge.Config.Bridge.Relay.FormatMessage(content, userID, *member)
	if err != nil {
		portal.log.Err(err).Msg("Failed to apply relaybot format")
	}
	content.FormattedBody = data
	// Force FileName field so the formatted body is used as a caption
	if content.FileName == "" {
		content.FileName = content.Body
	}
	return true
}

func (portal *Portal) sendEmailMessage(ctx context.Context, msg string, sender *User, evtID id.EventID) error {
	log := zerolog.Ctx(ctx).With().
		Str("action", "send email message").
		Stringer("event_id", evtID).
		Str("portal_chat_id", portal.ThreadID).
		Logger()
	ctx = log.WithContext(ctx)

	log.Debug().Msg("Sending event to Email")

	// Check to see if portal.ThreadID is an email address
	if portal.IsPrivateChat() {
		err := sender.Client.SendEmail(ctx, portal.EmailAddress, msg)
		if err != nil {
			return err
		}
	} else {
		// FIXME
		return errors.New("sending to email groups not supported yet")
	}

	log.Debug().Msg("Email sent successfully")
	return nil
}

func (portal *Portal) storeMessageInDB(ctx context.Context, eventID id.EventID, senderEmail string, timestamp uint64, partIndex int) {
	dbMessage := portal.bridge.DB.Message.New()
	dbMessage.MXID = eventID
	dbMessage.RoomID = portal.MXID
	dbMessage.Sender = senderEmail
	dbMessage.Timestamp = timestamp
	dbMessage.PartIndex = partIndex
	dbMessage.ThreadID = portal.ThreadID
	dbMessage.EmailReceiver = portal.Receiver
	err := dbMessage.Insert(ctx)
	if err != nil {
		portal.log.Err(err).Msg("Failed to insert message into database")
	}
}

// Bridge stuff related to Portals
func (br *IMAPBridge) dbPortalsToPortals(dbPortals []*database.Portal, err error) ([]*Portal, error) {
	if err != nil {
		return nil, err
	}

	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()

	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}

		portal, ok := br.portalsByID[dbPortal.PortalKey]
		if !ok {
			portal = br.loadPortal(context.TODO(), dbPortal, nil)
		}

		output[index] = portal
	}

	return output, nil
}

func (br *IMAPBridge) loadPortal(ctx context.Context, dbPortal *database.Portal, key *database.PortalKey) *Portal {
	if dbPortal == nil {
		if key == nil {
			return nil
		}

		dbPortal = br.DB.Portal.New()
		dbPortal.PortalKey = *key
		err := dbPortal.Insert(ctx)
		if err != nil {
			br.ZLog.Err(err).Msg("Failed to insert new portal")
			return nil
		}
	}

	portal := br.NewPortal(dbPortal)

	br.portalsByID[portal.PortalKey] = portal
	if portal.MXID != "" {
		br.portalsByMXID[portal.MXID] = portal
	}

	return portal
}

func (br *IMAPBridge) NewPortal(dbPortal *database.Portal) *Portal {

	log := br.ZLog.With().Str("thread_id", dbPortal.ThreadID).Logger()

	if dbPortal.MXID != "" {
		log = log.With().Stringer("room_id", dbPortal.MXID).Logger()
	}

	portal := &Portal{
		Portal: dbPortal,
		bridge: br,
		log:    log,

		emailMessages:  make(chan portalEmailMessage, br.Config.Bridge.PortalMessageBuffer),
		matrixMessages: make(chan portalMatrixMessage, br.Config.Bridge.PortalMessageBuffer),
	}

	go portal.messageLoop()

	return portal
}

func (portal *Portal) ensureUserInvited(ctx context.Context, user *User) bool {
	return user.ensureInvited(ctx, portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
}

func (portal *Portal) UpdateDMInfo(ctx context.Context, forceSave bool) {
	// FIXME
}

func (portal *Portal) CreateMatrixRoom(ctx context.Context, user *User, emailAddress string) error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if portal.MXID != "" {
		portal.log.Debug().Msg("Not creating room: already exists")
		return nil
	}
	portal.log.Debug().Msg("Creating matrix room")

	intent := portal.MainIntent()

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()
	initialState := []*event.Event{{
		Type:     event.StateBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     event.StateHalfShotBridge,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}

	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{Parsed: &event.RoomAvatarEventContent{
				URL: portal.AvatarURL,
			}},
		})
	}

	creationContent := make(map[string]interface{})
	if !portal.bridge.Config.Bridge.FederateRooms {
		creationContent["m.federate"] = false
	}

	var invite []id.UserID
	autoJoinInvites := portal.bridge.SpecVersions.Supports(mautrix.BeeperFeatureAutojoinInvites)
	if autoJoinInvites {
		invite = append(invite, user.MXID)
	}

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: portal.getEncryptionEventContent(),
			},
		})
		portal.Encrypted = true

		if portal.IsPrivateChat() && portal.MainIntent() != portal.bridge.Bot {
			invite = append(invite, portal.bridge.Bot.UserID)
		}
	}

	var dmPuppet *Puppet
	if portal.IsPrivateChat() {
		dmPuppet = portal.GetDMPuppet()
		if dmPuppet != nil {
			dmPuppet.UpdateInfo(ctx, user)
		}
	} else {
		portal.log.Warn().Msg("Not implemented yet")
	}

	req := &mautrix.ReqCreateRoom{
		Visibility:      "private",
		Name:            portal.Name,
		Topic:           portal.Topic,
		Invite:          invite,
		Preset:          "private_chat",
		IsDirect:        portal.IsPrivateChat(),
		InitialState:    initialState,
		CreationContent: creationContent,

		BeeperAutoJoinInvites: autoJoinInvites,
	}
	resp, err := intent.CreateRoom(ctx, req)
	if err != nil {
		portal.log.Warn().Err(err).Msg("failed to create room")
		return err
	}
	portal.log = portal.log.With().Stringer("room_id", resp.RoomID).Logger()

	portal.NameSet = len(req.Name) > 0
	portal.TopicSet = len(req.Topic) > 0
	portal.AvatarSet = !portal.AvatarURL.IsEmpty()
	portal.MXID = resp.RoomID
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()
	err = portal.Update(ctx)
	if err != nil {
		portal.log.Err(err).Msg("Failed to save portal room ID")
		return err
	}
	portal.log.Info().Msg("Created matrix room for portal")

	if !autoJoinInvites {
		if !portal.IsPrivateChat() {
			// portal.SyncParticipants(ctx, user, groupInfo)
		} else if portal.Encrypted {
			err = portal.bridge.Bot.EnsureJoined(ctx, portal.MXID, appservice.EnsureJoinedParams{BotOverride: portal.MainIntent().Client})
			if err != nil {
				portal.log.Error().Err(err).Msg("Failed to ensure bridge bot is joined to private chat portal")
			}
		}
		user.ensureInvited(ctx, portal.MainIntent(), portal.MXID, portal.IsPrivateChat())
	}
	user.syncChatDoublePuppetDetails(portal, true)
	// TODO: go portal.addToPersonalSpace(portal.log.WithContext(context.TODO()), user)

	if dmPuppet != nil {
		user.UpdateDirectChats(ctx, map[id.UserID][]id.RoomID{
			dmPuppet.MXID: {portal.MXID},
		})
	}

	return nil
}

func (br *IMAPBridge) FindPrivateChatPortalsWith(address string) []*Portal {
	portals, err := br.dbPortalsToPortals(br.DB.Portal.FindPrivateChatsWith(context.TODO(), address))
	if err != nil {
		br.ZLog.Err(err).Msg("Failed to get all DM portals with user")
		return nil
	}
	return portals
}

func (br *IMAPBridge) GetPortalByThreadID(key database.PortalKey) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	return br.unlockedGetPortalByThreadID(key, true)
}

func (br *IMAPBridge) unlockedGetPortalByThreadID(key database.PortalKey, createIfNotExists bool) *Portal {
	portal, ok := br.portalsByID[key]
	if !ok {
		dbPortal, err := br.DB.Portal.GetByThreadID(context.TODO(), key)
		if err != nil {
			br.ZLog.Err(err).Msg("Failed to get portal from database")
			return nil
		}
		keyIfNotExists := &key
		if !createIfNotExists {
			keyIfNotExists = nil
		}
		return br.loadPortal(context.TODO(), dbPortal, keyIfNotExists)
	}
	return portal
}

func (br *IMAPBridge) GetPortalByThreadIDIfExists(key database.PortalKey) *Portal {
	br.portalsLock.Lock()
	defer br.portalsLock.Unlock()
	return br.unlockedGetPortalByThreadID(key, false)
}

func (br *IMAPBridge) GetAllPortalsWithMXID() []*Portal {
	portals, err := br.dbPortalsToPortals(br.DB.Portal.GetAllWithMXID(context.TODO()))
	if err != nil {
		br.ZLog.Err(err).Msg("Failed to get all portals with mxid")
		return nil
	}
	return portals
}
