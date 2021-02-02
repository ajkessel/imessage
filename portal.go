// mautrix-imessage - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2021 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	log "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix/crypto/attachment"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-imessage/database"
	"go.mau.fi/mautrix-imessage/imessage"
)

func (bridge *Bridge) GetPortalByMXID(mxid id.RoomID) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByMXID[mxid]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByMXID(mxid), "")
	}
	return portal
}

func (bridge *Bridge) GetPortalByGUID(guid string) *Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	portal, ok := bridge.portalsByGUID[guid]
	if !ok {
		return bridge.loadDBPortal(bridge.DB.Portal.GetByGUID(guid), guid)
	}
	return portal
}

func (bridge *Bridge) GetAllPortals() []*Portal {
	return bridge.dbPortalsToPortals(bridge.DB.Portal.GetAll())
}

func (bridge *Bridge) dbPortalsToPortals(dbPortals []*database.Portal) []*Portal {
	bridge.portalsLock.Lock()
	defer bridge.portalsLock.Unlock()
	output := make([]*Portal, len(dbPortals))
	for index, dbPortal := range dbPortals {
		if dbPortal == nil {
			continue
		}
		portal, ok := bridge.portalsByGUID[dbPortal.GUID]
		if !ok {
			portal = bridge.loadDBPortal(dbPortal, "")
		}
		output[index] = portal
	}
	return output
}

func (bridge *Bridge) loadDBPortal(dbPortal *database.Portal, guid string) *Portal {
	if dbPortal == nil {
		if guid == "" {
			return nil
		}
		dbPortal = bridge.DB.Portal.New()
		dbPortal.GUID = guid
		dbPortal.Insert()
	}
	portal := bridge.NewPortal(dbPortal)
	bridge.portalsByGUID[portal.GUID] = portal
	if len(portal.MXID) > 0 {
		bridge.portalsByMXID[portal.MXID] = portal
	}
	return portal
}

func (bridge *Bridge) NewPortal(dbPortal *database.Portal) *Portal {
	portal := &Portal{
		Portal: dbPortal,
		bridge: bridge,
		log:    bridge.Log.Sub(fmt.Sprintf("Portal/%s", dbPortal.GUID)),

		Identifier:   imessage.ParseIdentifier(dbPortal.GUID),
		messageDedup: make(map[string]SentMessage),
	}
	return portal
}

type SentMessage struct {
	EventID   id.EventID
	Timestamp time.Time
}

type Portal struct {
	*database.Portal

	bridge *Bridge
	log    log.Logger

	roomCreateLock   sync.Mutex
	messageDedup     map[string]SentMessage
	messageDedupLock sync.Mutex
	Identifier       imessage.Identifier
}

func (portal *Portal) SyncParticipants() {
	members, err := portal.bridge.IM.GetGroupMembers(portal.GUID)
	if err != nil {
		portal.log.Errorln("Failed to get group members: %v", err)
		return
	}
	for _, member := range members {
		puppet := portal.bridge.GetPuppetByLocalID(member)
		puppet.Sync()
		err := puppet.Intent.EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Warnfln("Failed to make puppet of %s join %s: %v", member, portal.MXID, err)
		}
	}
}

func (portal *Portal) UpdateName(name string) bool {
	if portal.Name != name {
		intent := portal.MainIntent()
		_, err := intent.SetRoomName(portal.MXID, name)
		if err == nil {
			portal.Name = name
			portal.UpdateBridgeInfo()
			return true
		}
		portal.log.Warnln("Failed to set room name:", err)
	}
	return false
}

func (portal *Portal) Sync() {
	portal.log.Infoln("Syncing portal")

	if len(portal.MXID) == 0 {
		err := portal.CreateMatrixRoom()
		if err != nil {
			portal.log.Errorln("Failed to create portal room:", err)
		}
		return
	}

	if portal.bridge.user.DoublePuppetIntent != nil {
		_ = portal.bridge.user.DoublePuppetIntent.EnsureJoined(portal.MXID)
	}

	if portal.IsPrivateChat() {
		return
	}

	chatInfo, err := portal.bridge.IM.GetChatInfo(portal.GUID)
	if err != nil {
		portal.log.Errorln("Failed to get chat info: %v", err)
	}
	update := false
	if len(chatInfo.DisplayName) > 0 {
		update = portal.UpdateName(chatInfo.DisplayName)
	}
	portal.SyncParticipants()
	// TODO avatar?
	if update {
		portal.Update()
		portal.UpdateBridgeInfo()
	}
}

func (portal *Portal) GetBasePowerLevels() *event.PowerLevelsEventContent {
	anyone := 0
	nope := 99
	invite := 50
	return &event.PowerLevelsEventContent{
		UsersDefault:    anyone,
		EventsDefault:   anyone,
		RedactPtr:       &anyone,
		StateDefaultPtr: &nope,
		BanPtr:          &nope,
		InvitePtr:       &invite,
		Users: map[id.UserID]int{
			portal.MainIntent().UserID: 100,
		},
		Events: map[string]int{
			event.StateRoomName.Type:   anyone,
			event.StateRoomAvatar.Type: anyone,
			event.StateTopic.Type:      anyone,
		},
	}
}

type BridgeInfoSection struct {
	ID          string              `json:"id"`
	DisplayName string              `json:"displayname,omitempty"`
	AvatarURL   id.ContentURIString `json:"avatar_url,omitempty"`
	ExternalURL string              `json:"external_url,omitempty"`

	GUID    string `json:"fi.mau.imessage.guid"`
	Service string `json:"fi.mau.imessage.service"`
	IsGroup bool   `json:"fi.mau.imessage.is_group"`
}

type BridgeInfoContent struct {
	BridgeBot id.UserID          `json:"bridgebot"`
	Creator   id.UserID          `json:"creator,omitempty"`
	Protocol  BridgeInfoSection  `json:"protocol"`
	Network   *BridgeInfoSection `json:"network,omitempty"`
	Channel   BridgeInfoSection  `json:"channel"`
}

var (
	StateBridgeInfo         = event.Type{Type: "m.bridge", Class: event.StateEventType}
	StateHalfShotBridgeInfo = event.Type{Type: "uk.half-shot.bridge", Class: event.StateEventType}
)

func (portal *Portal) getBridgeInfo() (string, BridgeInfoContent) {
	bridgeInfo := BridgeInfoContent{
		BridgeBot: portal.bridge.Bot.UserID,
		Creator:   portal.MainIntent().UserID,
		Protocol: BridgeInfoSection{
			ID:          "imessage",
			DisplayName: "iMessage",
			AvatarURL:   id.ContentURIString(portal.bridge.Config.AppService.Bot.Avatar),
			ExternalURL: "https://support.apple.com/messages",
		},
		Channel: BridgeInfoSection{
			ID:          portal.Identifier.LocalID,
			DisplayName: portal.Name,
			AvatarURL:   portal.AvatarURL.CUString(),

			GUID:    portal.GUID,
			IsGroup: portal.Identifier.IsGroup,
			Service: portal.Identifier.Service,
		},
	}
	if portal.Identifier.Service == "SMS" {
		bridgeInfo.Protocol.ID = "imessage-sms"
		bridgeInfo.Protocol.DisplayName = "iMessage (SMS)"
	}
	bridgeInfoStateKey := fmt.Sprintf("net.maunium.imessage://%s/%s",
		strings.ToLower(portal.Identifier.Service), portal.GUID)
	return bridgeInfoStateKey, bridgeInfo
}

func (portal *Portal) UpdateBridgeInfo() {
	if len(portal.MXID) == 0 {
		portal.log.Debugln("Not updating bridge info: no Matrix room created")
		return
	}
	portal.log.Debugln("Updating bridge info...")
	stateKey, content := portal.getBridgeInfo()
	_, err := portal.MainIntent().SendStateEvent(portal.MXID, StateBridgeInfo, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update m.bridge:", err)
	}
	_, err = portal.MainIntent().SendStateEvent(portal.MXID, StateHalfShotBridgeInfo, stateKey, content)
	if err != nil {
		portal.log.Warnln("Failed to update uk.half-shot.bridge:", err)
	}
}

func (portal *Portal) CreateMatrixRoom() error {
	portal.roomCreateLock.Lock()
	defer portal.roomCreateLock.Unlock()
	if len(portal.MXID) > 0 {
		return nil
	}

	intent := portal.MainIntent()
	if err := intent.EnsureRegistered(); err != nil {
		return err
	}

	portal.log.Infoln("Creating Matrix room")

	chatInfo, err := portal.bridge.IM.GetChatInfo(portal.GUID)
	if err != nil {
		portal.log.Errorln("Failed to get chat info when creating portal: %v", err)
	}
	portal.Name = chatInfo.DisplayName

	if portal.IsPrivateChat() {
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		puppet.Sync()
		portal.Name = puppet.Displayname
		portal.AvatarURL = puppet.AvatarURL
		portal.Avatar = puppet.Avatar
	}

	bridgeInfoStateKey, bridgeInfo := portal.getBridgeInfo()

	initialState := []*event.Event{{
		Type: event.StatePowerLevels,
		Content: event.Content{
			Parsed: portal.GetBasePowerLevels(),
		},
	}, {
		Type:     StateBridgeInfo,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}, {
		// TODO remove this once https://github.com/matrix-org/matrix-doc/pull/2346 is in spec
		Type:     StateHalfShotBridgeInfo,
		Content:  event.Content{Parsed: bridgeInfo},
		StateKey: &bridgeInfoStateKey,
	}}
	if !portal.AvatarURL.IsEmpty() {
		initialState = append(initialState, &event.Event{
			Type: event.StateRoomAvatar,
			Content: event.Content{
				Parsed: event.RoomAvatarEventContent{URL: portal.AvatarURL},
			},
		})
	}

	invite := []id.UserID{portal.bridge.user.MXID}

	if portal.bridge.Config.Bridge.Encryption.Default {
		initialState = append(initialState, &event.Event{
			Type: event.StateEncryption,
			Content: event.Content{
				Parsed: event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1},
			},
		})
		portal.Encrypted = true
	}
	if portal.IsPrivateChat() {
		invite = append(invite, portal.bridge.Bot.UserID)
	}

	resp, err := intent.CreateRoom(&mautrix.ReqCreateRoom{
		Visibility:   "private",
		Name:         portal.Name,
		Invite:       invite,
		Preset:       "private_chat",
		IsDirect:     portal.IsPrivateChat(),
		InitialState: initialState,
	})
	if err != nil {
		return err
	}
	portal.MXID = resp.RoomID
	portal.Update()
	portal.bridge.portalsLock.Lock()
	portal.bridge.portalsByMXID[portal.MXID] = portal
	portal.bridge.portalsLock.Unlock()

	for _, user := range invite {
		portal.bridge.StateStore.SetMembership(portal.MXID, user, event.MembershipInvite)
	}

	if portal.Encrypted {
		err = portal.bridge.Bot.EnsureJoined(portal.MXID)
		if err != nil {
			portal.log.Errorln("Failed to join created portal with bridge bot for e2be:", err)
		}
	}

	if !portal.IsPrivateChat() {
		portal.SyncParticipants()
	} else {
		if portal.bridge.user.DoublePuppetIntent != nil {
			_ = portal.bridge.user.DoublePuppetIntent.EnsureJoined(portal.MXID)
		}
		puppet := portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID)
		portal.bridge.user.UpdateDirectChats(map[id.UserID][]id.RoomID{puppet.MXID: {portal.MXID}})
	}
	return nil
}

func (portal *Portal) IsPrivateChat() bool {
	return !portal.Identifier.IsGroup
}

func (portal *Portal) MainIntent() *appservice.IntentAPI {
	if portal.IsPrivateChat() {
		return portal.bridge.GetPuppetByLocalID(portal.Identifier.LocalID).Intent
	}
	return portal.bridge.Bot
}

func (portal *Portal) SetReply(content *event.MessageEventContent, msg *imessage.Message) {
	if len(msg.ReplyToGUID) == 0 {
		return
	}
	message := portal.bridge.DB.Message.GetByGUID(portal.GUID, msg.ReplyToGUID)
	if message != nil {
		evt, err := portal.MainIntent().GetEvent(portal.MXID, message.MXID)
		if err != nil {
			portal.log.Warnln("Failed to get reply target:", err)
			return
		}
		if evt.Type == event.EventEncrypted {
			_ = evt.Content.ParseRaw(evt.Type)
			decryptedEvt, err := portal.bridge.Crypto.Decrypt(evt)
			if err != nil {
				portal.log.Warnln("Failed to decrypt reply target:", err)
			} else {
				evt = decryptedEvt
			}
		}
		_ = evt.Content.ParseRaw(evt.Type)
		content.SetReply(evt)
	}
	return
}

func (portal *Portal) sendMainIntentMessage(content interface{}) (*mautrix.RespSendEvent, error) {
	return portal.sendMessage(portal.MainIntent(), event.EventMessage, content, 0)
}

const MessageSendRetries = 5
const MediaUploadRetries = 5
const BadGatewaySleep = 5 * time.Second

func (portal *Portal) sendMessage(intent *appservice.IntentAPI, eventType event.Type, content interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	return portal.sendMessageWithRetry(intent, eventType, content, timestamp, MessageSendRetries)
}

func isGatewayError(err error) bool {
	if err == nil {
		return false
	}
	var httpErr mautrix.HTTPError
	return errors.As(err, &httpErr) && (httpErr.IsStatus(http.StatusBadGateway) || httpErr.IsStatus(http.StatusGatewayTimeout))
}

func (portal *Portal) sendMessageWithRetry(intent *appservice.IntentAPI, eventType event.Type, content interface{}, timestamp int64, retries int) (*mautrix.RespSendEvent, error) {
	for ; ; retries-- {
		resp, err := portal.sendMessageDirect(intent, eventType, content, timestamp)
		if retries > 0 && isGatewayError(err) {
			portal.log.Warnfln("Got gateway error trying to send message, retrying in %d seconds", int(BadGatewaySleep.Seconds()))
			time.Sleep(BadGatewaySleep)
		} else {
			return resp, err
		}
	}
}

func (portal *Portal) sendMessageDirect(intent *appservice.IntentAPI, eventType event.Type, content interface{}, timestamp int64) (*mautrix.RespSendEvent, error) {
	wrappedContent := event.Content{Parsed: content}
	if timestamp != 0 && intent.IsCustomPuppet {
		wrappedContent.Raw = map[string]interface{}{
			"net.maunium.imessage.puppet": intent.IsCustomPuppet,
		}
	}
	if portal.Encrypted && portal.bridge.Crypto != nil {
		encrypted, err := portal.bridge.Crypto.Encrypt(portal.MXID, eventType, wrappedContent)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt event: %w", err)
		}
		eventType = event.EventEncrypted
		wrappedContent.Parsed = encrypted
	}
	if timestamp == 0 {
		return intent.SendMessageEvent(portal.MXID, eventType, &wrappedContent)
	} else {
		return intent.SendMassagedMessageEvent(portal.MXID, eventType, &wrappedContent, timestamp)
	}
}

func (portal *Portal) encryptFile(data []byte, mimeType string) ([]byte, string, *event.EncryptedFileInfo) {
	if !portal.Encrypted {
		return data, mimeType, nil
	}

	file := &event.EncryptedFileInfo{
		EncryptedFile: *attachment.NewEncryptedFile(),
		URL:           "",
	}
	return file.Encrypt(data), "application/octet-stream", file
}

func (portal *Portal) uploadWithRetry(intent *appservice.IntentAPI, data []byte, mimeType string, retries int) (*mautrix.RespMediaUpload, error) {
	for ; ; retries-- {
		uploaded, err := intent.UploadBytes(data, mimeType)
		if isGatewayError(err) {
			portal.log.Warnfln("Got gateway error trying to upload media, retrying in %d seconds", int(BadGatewaySleep.Seconds()))
			time.Sleep(BadGatewaySleep)
		} else {
			return uploaded, err
		}
	}
}

func (portal *Portal) sendErrorMessage(message string) id.EventID {
	resp, err := portal.sendMainIntentMessage(event.MessageEventContent{
		MsgType: event.MsgNotice,
		Body:    fmt.Sprintf("\u26a0 Your message may not have been bridged: %v", message),
	})
	if err != nil {
		portal.log.Warnfln("Failed to send bridging error message:", err)
		return ""
	}
	return resp.EventID
}

func (portal *Portal) HandleMatrixMessage(evt *event.Event) {
	msg, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		// TODO log
		return
	}
	if msg.MsgType == event.MsgText {
		portal.messageDedupLock.Lock()
		portal.messageDedup[msg.Body] = SentMessage{
			EventID:   evt.ID,
			Timestamp: time.Now(),
		}
		portal.messageDedupLock.Unlock()
		// TODO deduplicate sent messages
		err := portal.bridge.IM.SendMessage(portal.GUID, msg.Body)
		if err != nil {
			portal.log.Errorln("Error sending to iMessage: %v", err)
		} else {
			portal.log.Debugln("Handled Matrix message", evt.ID)
		}
	}
}

func (portal *Portal) HandleMatrixReaction(evt *event.Event) {

}

func (portal *Portal) HandleiMessage(msg *imessage.Message) {
	dbMessage := portal.bridge.DB.Message.New()
	dbMessage.ChatGUID = portal.GUID
	dbMessage.SenderGUID = msg.Sender.String()
	dbMessage.GUID = msg.GUID
	dbMessage.Timestamp = msg.Time.UnixNano() / int64(time.Millisecond)
	var intent *appservice.IntentAPI
	if msg.IsFromMe {
		intent = portal.bridge.user.DoublePuppetIntent
		if intent == nil {
			portal.log.Debugfln("Dropping own message in %s as double puppeting is not initialized", msg.ChatGUID)
			return
		}
		portal.messageDedupLock.Lock()
		dedup, isDup := portal.messageDedup[msg.Text]
		if isDup && dedup.Timestamp.Before(msg.Time) {
			delete(portal.messageDedup, msg.Text)
			portal.messageDedupLock.Unlock()
			portal.log.Debugfln("Received echo for Matrix message %s -> %s", dedup.EventID, msg.GUID)
			dbMessage.MXID = dedup.EventID
			dbMessage.Insert()
			return
		} else {
			portal.messageDedupLock.Unlock()
		}
	} else {
		puppet := portal.bridge.GetPuppetByLocalID(msg.Sender.LocalID)
		intent = puppet.Intent
	}
	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    msg.Text,
	}
	portal.SetReply(content, msg)
	resp, err := portal.sendMessage(intent, event.EventMessage, content, dbMessage.Timestamp)
	if err != nil {
		portal.log.Errorln("Failed to send message from %s: %v", msg.Sender.LocalID, err)
		return
	}
	portal.log.Debugfln("Handled iMessage %s -> %s", msg.GUID, resp.EventID)
	dbMessage.MXID = resp.EventID
	dbMessage.Insert()
}

func (portal *Portal) HandleiMessageTapback(msg *imessage.Message) {
	target := portal.bridge.DB.Message.GetByGUID(portal.GUID, msg.Tapback.TargetGUID)
	if target == nil {
		portal.log.Debugln("Unknown tapback target", msg.Tapback.TargetGUID)
		return
	}
	puppet := portal.bridge.GetPuppetByLocalID(msg.Sender.LocalID)
	senderGUID := msg.Sender.String()

	resp, err := puppet.Intent.SendReaction(portal.MXID, target.MXID, msg.Tapback.Type.Emoji())
	if err != nil {
		portal.log.Errorln("Failed to send tapback from %s: %v", msg.Sender.LocalID)
		return
	}

	existing := portal.bridge.DB.Tapback.GetByGUID(portal.GUID, target.GUID, senderGUID)
	if existing == nil {
		tapback := portal.bridge.DB.Tapback.New()
		tapback.ChatGUID = portal.GUID
		tapback.MessageGUID = target.GUID
		tapback.SenderGUID = senderGUID
		tapback.Type = msg.Tapback.Type
		tapback.MXID = resp.EventID
		tapback.Insert()
	} else {
		_, err = puppet.Intent.RedactEvent(portal.MXID, existing.MXID)
		if err != nil {
			portal.log.Warnln("Failed to redact old tapback from %s: %v", msg.Sender.LocalID, err)
		}
		existing.Type = msg.Tapback.Type
		existing.MXID = resp.EventID
		existing.Update()
	}
}

func (portal *Portal) Delete() {
	portal.Portal.Delete()
	portal.bridge.portalsLock.Lock()
	delete(portal.bridge.portalsByGUID, portal.GUID)
	if len(portal.MXID) > 0 {
		delete(portal.bridge.portalsByMXID, portal.MXID)
	}
	portal.bridge.portalsLock.Unlock()
}

func (portal *Portal) GetMatrixUsers() ([]id.UserID, error) {
	members, err := portal.MainIntent().JoinedMembers(portal.MXID)
	if err != nil {
		return nil, fmt.Errorf("failed to get member list: %w", err)
	}
	var users []id.UserID
	for userID := range members.Joined {
		_, isPuppet := portal.bridge.ParsePuppetMXID(userID)
		if !isPuppet && userID != portal.bridge.Bot.UserID {
			users = append(users, userID)
		}
	}
	return users, nil
}

func (portal *Portal) CleanupIfEmpty() {
	users, err := portal.GetMatrixUsers()
	if err != nil {
		portal.log.Errorfln("Failed to get Matrix user list to determine if portal needs to be cleaned up: %v", err)
		return
	}

	if len(users) == 0 {
		portal.log.Infoln("Room seems to be empty, cleaning up...")
		portal.Delete()
		portal.Cleanup(false)
	}
}

func (portal *Portal) Cleanup(puppetsOnly bool) {
	if len(portal.MXID) == 0 {
		return
	}
	if portal.IsPrivateChat() {
		_, err := portal.MainIntent().LeaveRoom(portal.MXID)
		if err != nil {
			portal.log.Warnln("Failed to leave private chat portal with main intent:", err)
		}
		return
	}
	intent := portal.MainIntent()
	members, err := intent.JoinedMembers(portal.MXID)
	if err != nil {
		portal.log.Errorln("Failed to get portal members for cleanup:", err)
		return
	}
	for member := range members.Joined {
		if member == intent.UserID {
			continue
		}
		puppet := portal.bridge.GetPuppetByMXID(member)
		if puppet != nil {
			_, err = puppet.Intent.LeaveRoom(portal.MXID)
			if err != nil {
				portal.log.Errorln("Error leaving as puppet while cleaning up portal:", err)
			}
		} else if !puppetsOnly {
			_, err = intent.KickUser(portal.MXID, &mautrix.ReqKickUser{UserID: member, Reason: "Deleting portal"})
			if err != nil {
				portal.log.Errorln("Error kicking user while cleaning up portal:", err)
			}
		}
	}
	_, err = intent.LeaveRoom(portal.MXID)
	if err != nil {
		portal.log.Errorln("Error leaving with main intent while cleaning up portal:", err)
	}
}