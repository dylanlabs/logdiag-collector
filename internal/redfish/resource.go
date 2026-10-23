// SPDX-License-Identifier: GPL-3.0-or-later

package redfish

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

// Resource 是系统、管理器等资源里识别设备和查找动作用到的字段。
type Resource struct {
	ODataID         string `json:"@odata.id"`
	ID              string `json:"Id"`
	Name            string
	Manufacturer    string
	Model           string
	SerialNumber    string
	HostName        string
	ManagerType     string
	FirmwareVersion string
	BiosVersion     string
	PowerState      string
	Actions         json.RawMessage
	LogServices     ODataID
	Oem             map[string]json.RawMessage
}

// Members 读取集合，返回成员地址。
func (c *Client) Members(ctx context.Context, uri string) ([]string, error) {
	var coll struct {
		Members []ODataID
	}
	if _, err := c.Get(ctx, uri, &coll); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range coll.Members {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Action 是资源 Actions 里的一个动作。
type Action struct {
	Name   string // 例如 "#Manager.Dump"
	Target string
	// Vendor 是 OEM 动作所在的厂商键，例如 "Huawei"；标准动作为空。
	Vendor string
	// Allowable 是各参数的 @Redfish.AllowableValues，真机核对参数时有用。
	Allowable map[string][]string
}

// ParseActions 列出 Actions 里的标准动作和 Oem 下各厂商的动作，按名称排序。
func ParseActions(raw json.RawMessage) []Action {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	var out []Action
	add := func(name, vendor string, body json.RawMessage) {
		var fields map[string]json.RawMessage
		json.Unmarshal(body, &fields)
		a := Action{Name: name, Vendor: vendor}
		json.Unmarshal(fields["target"], &a.Target)
		for k, v := range fields {
			param, ok := strings.CutSuffix(k, "@Redfish.AllowableValues")
			if !ok {
				continue
			}
			var values []string
			if json.Unmarshal(v, &values) == nil {
				if a.Allowable == nil {
					a.Allowable = map[string][]string{}
				}
				a.Allowable[param] = values
			}
		}
		out = append(out, a)
	}
	for k, v := range top {
		if strings.HasPrefix(k, "#") {
			add(k, "", v)
			continue
		}
		if k != "Oem" {
			continue
		}
		var oem map[string]json.RawMessage
		json.Unmarshal(v, &oem)
		for k2, v2 := range oem {
			if strings.HasPrefix(k2, "#") {
				add(k2, "", v2)
				continue
			}
			var vendor map[string]json.RawMessage
			json.Unmarshal(v2, &vendor)
			for k3, v3 := range vendor {
				if strings.HasPrefix(k3, "#") {
					add(k3, k2, v3)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b Action) int {
		return strings.Compare(a.Vendor+a.Name, b.Vendor+b.Name)
	})
	return out
}

// FindAction 按名称查找动作，name 可以带或不带开头的 "#"，例如 "Manager.Dump"。
// 厂商键不写死：超聚变新固件的 OEM 键可能不是 "Huawei"。
func FindAction(actions []Action, name string) (Action, bool) {
	name = "#" + strings.TrimPrefix(name, "#")
	for _, a := range actions {
		if a.Name == name && a.Target != "" {
			return a, true
		}
	}
	return Action{}, false
}
