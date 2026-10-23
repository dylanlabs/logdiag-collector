// SPDX-License-Identifier: GPL-3.0-or-later

package ui

import (
	"encoding/json"
	"sync"
)

// Hub 把后台的变化（任务进度、设备状态）推给所有打开的界面窗口。
type Hub struct {
	mu   sync.Mutex
	subs map[chan event]struct{}
}

type event struct {
	name string
	data []byte
}

// subscriberBuffer 是每个窗口积压事件的上限。积压满了说明窗口卡住了：断开它，
// 浏览器会自动重连，重连后界面重新读取全部数据，不会漏掉变化。
const subscriberBuffer = 512

// NewHub 创建事件中心。
func NewHub() *Hub {
	return &Hub{subs: map[chan event]struct{}{}}
}

// Publish 发送一个事件，data 编码成 JSON。不会阻塞。
func (h *Hub) Publish(name string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	ev := event{name, b}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *Hub) subscribe() (<-chan event, func()) {
	ch := make(chan event, subscriberBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}
