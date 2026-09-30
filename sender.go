package main

import (
	"context"
	"log/slog"
	"time"

	meshcore "github.com/meshcore-go/meshcore-go"
	"github.com/meshcore-go/meshcore-go/node"
)

type NodeSender struct {
	node *node.Node
	log  *slog.Logger
}

func NewNodeSender(n *node.Node) *NodeSender {
	return &NodeSender{
		node: n,
		log:  slog.Default().With("component", "sender", "type", "node"),
	}
}

func (s *NodeSender) SendGroupText(_ context.Context, channel *meshcore.ChannelEntry, senderName string, text string, pathHashSize uint8, retryTimeout time.Duration, maxRetries int) error {
	reply := &meshcore.GroupTextPayload{
		Timestamp: uint32(time.Now().Unix()),
		Sender:    senderName,
		Text:      text,
	}

	return s.node.SendGroupText(
		channel,
		reply,
		pathHashSize,
		retryTimeout,
		maxRetries,
		func(gsr node.GroupSendResult) {
			s.log.Debug("GroupSendResult", "text", text, "Confimed", gsr.Confirmed)
		})
}
