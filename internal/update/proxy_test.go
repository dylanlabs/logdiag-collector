// SPDX-License-Identifier: GPL-3.0-or-later

package update

import (
	"net/url"
	"testing"
)

func TestProxySettings(t *testing.T) {
	logdiag, _ := url.Parse("https://logdiag.com/collector/latest.json")
	intranet, _ := url.Parse("https://wiki/collector/latest.json")
	for _, c := range []struct {
		server, override string
		u                *url.URL
		want             string
	}{
		{"10.1.1.1:8080", "", logdiag, "http://10.1.1.1:8080"},
		{"proxy.corp:3128", "<local>", intranet, ""},
		{"proxy.corp:3128", "<local>", logdiag, "http://proxy.corp:3128"},
		{"proxy.corp:3128", "*.corp;*.logdiag.com;logdiag.com", logdiag, ""},
		{"proxy.corp:3128", "https://logdiag.*", logdiag, ""},
		{"http=a:80;https=b:443;socks=c:1080", "", logdiag, "http://b:443"},
		{"http=a:80;socks=c:1080", "", logdiag, "http://a:80"},
		{"socks=c:1080", "", logdiag, ""},
		{"https://secure.proxy:443", "", logdiag, "https://secure.proxy:443"},
		{"", "", logdiag, ""},
	} {
		p := &proxySettings{Server: c.server, Override: c.override}
		got, err := p.forURL(c.u)
		if err != nil {
			t.Errorf("%+v：%v", c, err)
			continue
		}
		if s := ""; got != nil {
			s = got.String()
			if s != c.want {
				t.Errorf("代理 %q 例外 %q 访问 %s：%s，应为 %q", c.server, c.override, c.u.Host, s, c.want)
			}
		} else if c.want != "" {
			t.Errorf("代理 %q 例外 %q 访问 %s：不走代理，应为 %q", c.server, c.override, c.u.Host, c.want)
		}
	}
	var none *proxySettings
	if got, _ := none.forURL(logdiag); got != nil {
		t.Errorf("没有设置代理：%v", got)
	}
}
