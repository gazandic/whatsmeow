// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package multidevice

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/RadicalApp/libsignal-protocol-go/groups"
	"github.com/RadicalApp/libsignal-protocol-go/keys/prekey"
	"github.com/RadicalApp/libsignal-protocol-go/protocol"
	"github.com/RadicalApp/libsignal-protocol-go/session"

	whatsapp "go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	waProto "go.mau.fi/whatsmeow/binary/proto"
)

func GenerateMessageID() string {
	id := make([]byte, 16)
	_, err := rand.Read(id)
	if err != nil {
		// Out of entropy
		panic(err)
	}
	return hex.EncodeToString(id)
}

func (cli *Client) SendMessage(to waBinary.FullJID, id string, message *waProto.Message) error {
	if to.AD {
		return fmt.Errorf("message recipient must be non-AD JID")
	}

	if len(id) == 0 {
		id = GenerateMessageID()
	}

	if to.Server == waBinary.GroupServer {
		return cli.sendGroup(to, id, message)
	} else {
		return cli.sendDM(to, id, message)
	}
}

func participantListHashV2(participantJIDs []string) string {
	sort.Strings(participantJIDs)
	hash := sha256.Sum256([]byte(strings.Join(participantJIDs, "")))
	return fmt.Sprintf("2:%s", base64.RawStdEncoding.EncodeToString(hash[:6]))
}

func (cli *Client) sendGroup(to waBinary.FullJID, id string, message *waProto.Message) error {
	groupInfo, err := cli.GetGroupInfo(to)
	if err != nil {
		return fmt.Errorf("failed to get group info: %w", err)
	}

	plaintext, _, err := marshalMessage(to, message)
	if err != nil {
		return err
	}

	builder := groups.NewGroupSessionBuilder(cli.Session, pbSerializer)
	senderKeyName := protocol.NewSenderKeyName(to.String(), cli.Session.ID.SignalAddress())
	signalSKDMessage, err := builder.Create(senderKeyName)
	if err != nil {
		return fmt.Errorf("failed to create sender key distribution message to send %s to %s: %w", id, to, err)
	}
	skdMessage := &waProto.Message{
		SenderKeyDistributionMessage: &waProto.SenderKeyDistributionMessage{
			GroupId:                             proto.String(to.String()),
			AxolotlSenderKeyDistributionMessage: signalSKDMessage.Serialize(),
		},
	}
	skdPlaintext, err := proto.Marshal(skdMessage)
	if err != nil {
		return fmt.Errorf("failed to marshal sender key distribution message to send %s to %s: %w", id, to, err)
	}

	cipher := groups.NewGroupCipher(builder, senderKeyName, cli.Session)
	encrypted, err := cipher.Encrypt(padMessage(plaintext))
	if err != nil {
		return fmt.Errorf("failed to encrypt group message to send %s to %s: %w", id, to, err)
	}
	ciphertext := encrypted.SignedSerialize()

	participants := make([]waBinary.FullJID, len(groupInfo.Participants))
	participantsStrings := make([]string, len(groupInfo.Participants))
	for i, part := range groupInfo.Participants {
		participants[i] = part.FullJID
		participantsStrings[i] = part.FullJID.String()
	}

	allDevices, err := cli.GetUSyncDevices(participants, false)
	if err != nil {
		return fmt.Errorf("failed to get device list: %w", err)
	}
	participantNodes, includeIdentity := cli.encryptMessageForDevices(allDevices, id, skdPlaintext, nil)

	node := waBinary.Node{
		Tag: "message",
		Attrs: map[string]interface{}{
			"id":    id,
			"type":  "text",
			"to":    to,
			"phash": participantListHashV2(participantsStrings),
		},
		Content: []waBinary.Node{
			{Tag: "participants", Content: participantNodes},
			{Tag: "enc", Content: ciphertext, Attrs: map[string]interface{}{"v": "2", "type": "skmsg"}},
		},
	}
	if includeIdentity {
		err = cli.appendDeviceIdentityNode(&node)
		if err != nil {
			return err
		}
	}
	err = cli.sendNode(node)
	if err != nil {
		return fmt.Errorf("failed to send message node: %w", err)
	}
	return nil
}

func (cli *Client) sendDM(to waBinary.FullJID, id string, message *waProto.Message) error {
	messagePlaintext, deviceSentMessagePlaintext, err := marshalMessage(to, message)
	if err != nil {
		return err
	}

	allDevices, err := cli.GetUSyncDevices([]waBinary.FullJID{to, *cli.Session.ID}, false)
	if err != nil {
		return fmt.Errorf("failed to get device list: %w", err)
	}
	participantNodes, includeIdentity := cli.encryptMessageForDevices(allDevices, id, messagePlaintext, deviceSentMessagePlaintext)

	node := waBinary.Node{
		Tag: "message",
		Attrs: map[string]interface{}{
			"id":   id,
			"type": "text",
			"to":   to,
		},
		Content: []waBinary.Node{{
			Tag:     "participants",
			Content: participantNodes,
		}},
	}
	if includeIdentity {
		err = cli.appendDeviceIdentityNode(&node)
		if err != nil {
			return err
		}
	}
	err = cli.sendNode(node)
	if err != nil {
		return fmt.Errorf("failed to send message node: %w", err)
	}
	return nil
}

func marshalMessage(to waBinary.FullJID, message *waProto.Message) (plaintext, dsmPlaintext []byte, err error) {
	plaintext, err = proto.Marshal(message)
	if err != nil {
		err = fmt.Errorf("failed to marshal message: %w", err)
		return
	}

	if to.Server != waBinary.GroupServer {
		dsmPlaintext, err = proto.Marshal(&waProto.DeviceSentMessage{
			DestinationJid: proto.String(to.String()),
			Message:        message,
		})
		if err != nil {
			err = fmt.Errorf("failed to marshal message (for own devices): %w", err)
			return
		}
	}

	return
}

func (cli *Client) GetGroupInfo(jid waBinary.FullJID) (*whatsapp.GroupInfo, error) {
	res, err := cli.sendIQ(InfoQuery{
		Namespace: "w:g2",
		Type:      "get",
		To:        jid,
		Content: []waBinary.Node{{
			Tag:   "query",
			Attrs: map[string]interface{}{"request": "interactive"},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to request group info: %w", err)
	}

	errorNode, ok := res.GetOptionalChildByTag("error")
	if ok {
		return nil, fmt.Errorf("group info request returned error: %s", errorNode.XMLString())
	}

	groupNode, ok := res.GetOptionalChildByTag("group")
	if !ok {
		return nil, fmt.Errorf("group info request didn't return group info")
	}

	var group whatsapp.GroupInfo
	ag := groupNode.AttrGetter()

	group.JID = waBinary.NewJID(ag.String("id"), waBinary.GroupServer).String()
	group.OwnerJID = ag.JID("creator").String()

	group.Name = ag.String("subject")
	group.NameSetTime = ag.Int64("s_t")
	group.NameSetBy = ag.JID("s_o").String()

	group.GroupCreated = ag.Int64("creation")

	for _, child := range groupNode.GetChildren() {
		childAG := child.AttrGetter()
		switch child.Tag {
		case "participant":
			participant := whatsapp.GroupParticipant{
				IsAdmin: childAG.OptionalString("type") == "admin",
				FullJID: childAG.JID("jid"),
			}
			participant.JID = participant.FullJID.String()
			group.Participants = append(group.Participants, participant)
		case "description":
			body, bodyOK := child.GetOptionalChildByTag("body")
			if bodyOK {
				group.Topic, _ = body.Content.(string)
				group.TopicID = childAG.String("id")
				group.TopicSetBy = childAG.JID("participant").String()
				group.TopicSetAt = childAG.Int64("t")
			}
		case "announcement":
			group.Announce = true
		case "locked":
			group.Locked = true
		default:
			cli.Log.Debugfln("Unknown element in group node %s: %s", jid.String(), child.XMLString())
		}
		if !childAG.OK() {
			cli.Log.Warnfln("Possibly failed to parse %s element in group node: %+v", child.Tag, childAG.Errors)
		}
	}

	return &group, nil
}

func (cli *Client) GetUSyncDevices(jids []waBinary.FullJID, ignorePrimary bool) ([]waBinary.FullJID, error) {
	userList := make([]waBinary.Node, len(jids))
	for i, jid := range jids {
		userList[i].Tag = "user"
		userList[i].Attrs = map[string]interface{}{"jid": waBinary.NewJID(jid.User, waBinary.DefaultUserServer)}
	}
	res, err := cli.sendIQ(InfoQuery{
		Namespace: "usync",
		Type:      "get",
		To:        waBinary.ServerJID,
		Content: []waBinary.Node{{
			Tag: "usync",
			Attrs: map[string]interface{}{
				"sid":     cli.generateRequestID(),
				"mode":    "query",
				"last":    "true",
				"index":   "0",
				"context": "message",
			},
			Content: []waBinary.Node{
				{Tag: "query", Content: []waBinary.Node{{
					Tag: "devices",
					Attrs: map[string]interface{}{
						"version": "2",
					},
				}}},
				{Tag: "list", Content: userList},
			},
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to send usync query: %w", err)
	}
	usync := res.GetChildByTag("usync")
	if usync.Tag != "usync" {
		return nil, fmt.Errorf("unexpected children in response to usync query")
	}
	list := usync.GetChildByTag("list")
	if list.Tag != "list" {
		return nil, fmt.Errorf("missing list inside usync tag")
	}

	var devices []waBinary.FullJID
	for _, user := range list.GetChildren() {
		jid, jidOK := user.Attrs["jid"].(waBinary.FullJID)
		if user.Tag != "user" || !jidOK {
			continue
		}
		deviceNode := user.GetChildByTag("devices")
		deviceList := deviceNode.GetChildByTag("device-list")
		if deviceNode.Tag != "devices" || deviceList.Tag != "device-list" {
			continue
		}
		for _, device := range deviceList.GetChildren() {
			deviceID, ok := device.AttrGetter().GetInt64("id", true)
			if device.Tag != "device" || !ok {
				continue
			}
			deviceJID := waBinary.NewADJID(jid.User, 0, byte(deviceID))
			if (deviceJID.Device > 0 || !ignorePrimary) && deviceJID != *cli.Session.ID {
				devices = append(devices, deviceJID)
			}
		}
	}

	return devices, nil
}

func (cli *Client) appendDeviceIdentityNode(node *waBinary.Node) error {
	deviceIdentity, err := proto.Marshal(cli.Session.Account)
	if err != nil {
		return fmt.Errorf("failed to marshal device identity: %w", err)
	}
	node.Content = append(node.GetChildren(), waBinary.Node{
		Tag:     "device-identity",
		Content: deviceIdentity,
	})
	return nil
}

func (cli *Client) encryptMessageForDevices(allDevices []waBinary.FullJID, id string, msgPlaintext, dsmPlaintext []byte) ([]waBinary.Node, bool) {
	includeIdentity := false
	participantNodes := make([]waBinary.Node, 0, len(allDevices))
	var retryDevices []waBinary.FullJID
	for _, jid := range allDevices {
		plaintext := msgPlaintext
		if jid.User == cli.Session.ID.User && dsmPlaintext != nil {
			plaintext = dsmPlaintext
		}
		encrypted, isPreKey, err := cli.encryptMessageForDevice(plaintext, jid, nil)
		if errors.Is(err, ErrNoSession) {
			retryDevices = append(retryDevices, jid)
			continue
		} else if err != nil {
			cli.Log.Warnfln("Failed to encrypt %s for %s: %v", id, jid, err)
			continue
		}
		participantNodes = append(participantNodes, *encrypted)
		if isPreKey {
			includeIdentity = true
		}
	}
	if len(retryDevices) > 0 {
		bundles, err := cli.fetchPreKeys(retryDevices)
		if err != nil {
			cli.Log.Warnln("Failed to fetch prekeys for", retryDevices, "to retry encryption:", err)
		} else {
			for _, jid := range retryDevices {
				resp := bundles[jid]
				if resp.err != nil {
					cli.Log.Warnfln("Failed to fetch prekey for %s: %v", jid, resp.err)
					continue
				}
				plaintext := msgPlaintext
				if jid.User == cli.Session.ID.User && dsmPlaintext != nil {
					plaintext = dsmPlaintext
				}
				encrypted, isPreKey, err := cli.encryptMessageForDevice(plaintext, jid, resp.bundle)
				if err != nil {
					cli.Log.Warnfln("Failed to encrypt %s for %s (retry): %v", id, jid, err)
					continue
				}
				participantNodes = append(participantNodes, *encrypted)
				if isPreKey {
					includeIdentity = true
				}
			}
		}
	}
	return participantNodes, includeIdentity
}

var ErrNoSession = errors.New("no signal session established")

func (cli *Client) encryptMessageForDevice(plaintext []byte, to waBinary.FullJID, bundle *prekey.Bundle) (*waBinary.Node, bool, error) {
	builder := session.NewBuilderFromSignal(cli.Session, to.SignalAddress(), pbSerializer)
	if !cli.Session.ContainsSession(to.SignalAddress()) {
		if bundle != nil {
			cli.Log.Debugln("Processing prekey bundle for", to)
			err := builder.ProcessBundle(bundle)
			if err != nil {
				return nil, false, fmt.Errorf("failed to process prekey bundle: %w", err)
			}
		} else {
			return nil, false, ErrNoSession
		}
	}
	cipher := session.NewCipher(builder, to.SignalAddress())
	ciphertext, err := cipher.Encrypt(padMessage(plaintext))
	if err != nil {
		return nil, false, fmt.Errorf("cipher encryption failed: %w", err)
	}

	encType := "msg"
	if ciphertext.Type() == protocol.PREKEY_TYPE {
		encType = "pkmsg"
	}

	return &waBinary.Node{
		Tag: "to",
		Attrs: map[string]interface{}{
			"jid": to,
		},
		Content: []waBinary.Node{{
			Tag: "enc",
			Attrs: map[string]interface{}{
				"v":    "2",
				"type": encType,
			},
			Content: ciphertext.Serialize(),
		}},
	}, encType == "pkmsg", nil
}