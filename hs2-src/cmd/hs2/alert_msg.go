package main

// Alert message texts (Persian by default, English with lang=en) and the
// Telegram sender. Messages are short: one line of what, one of numbers.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type alertTexts struct{ fa bool }

func alertText(lang string) alertTexts { return alertTexts{fa: lang != "en"} }

func (t alertTexts) pick(fa, en string) string {
	if t.fa {
		return fa
	}
	return en
}

func dur(d time.Duration, fa bool) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 1 {
		if fa {
			return "کمتر از ۱ دقیقه"
		}
		return "<1 min"
	}
	if m < 60 {
		if fa {
			return fmt.Sprintf("%d دقیقه", m)
		}
		return fmt.Sprintf("%d min", m)
	}
	if fa {
		return fmt.Sprintf("%d ساعت و %d دقیقه", m/60, m%60)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

func (t alertTexts) resolved(text string, d time.Duration) string {
	return t.pick("برطرف شد ("+dur(d, true)+"): ", "resolved (after "+dur(d, false)+"): ") + text
}

func (t alertTexts) still(text string, d time.Duration) string {
	return t.pick("هنوز ادامه دارد ("+dur(d, true)+"): ", "still on ("+dur(d, false)+"): ") + text
}

func (t alertTexts) unitDown(unit, state string) string {
	if state == "" {
		state = "unknown"
	}
	return t.pick(fmt.Sprintf("سرویس %s فعال نیست (%s)", unit, state), fmt.Sprintf("service %s is not active (%s)", unit, state))
}

func (t alertTexts) restarted(unit string, n int) string {
	if n >= 0 {
		return t.pick(fmt.Sprintf("سرویس %s دوباره راه‌اندازی شد (شمار ری‌استارت خودکار: %d)", unit, n), fmt.Sprintf("service %s restarted (auto-restarts: %d)", unit, n))
	}
	return t.pick(fmt.Sprintf("سرویس %s دوباره راه‌اندازی شد", unit), fmt.Sprintf("service %s restarted", unit))
}

func (t alertTexts) panicked(unit string, n int, line string) string {
	return t.pick(fmt.Sprintf("خطای panic/fatal در %s (%d خط): %s", unit, n, line), fmt.Sprintf("panic/fatal in %s (%d line(s)): %s", unit, n, line))
}

func (t alertTexts) dead(users int) string {
	return t.pick(fmt.Sprintf("هیچ لینکی سرویس نمی‌دهد ولی %d کاربر وصل است", users), fmt.Sprintf("no serving link while %d user(s) are connected", users))
}

func (t alertTexts) linksLow(serving, target int) string {
	return t.pick(fmt.Sprintf("لینک‌های فعال کم است: %d از %d", serving, target), fmt.Sprintf("serving links low: %d of %d", serving, target))
}

func (t alertTexts) ipCut(ip string, paths int, reason string) string {
	return t.pick(fmt.Sprintf("آی‌پی %s کامل قطع است (همه %d مسیر؛ %s)", ip, paths, reason), fmt.Sprintf("IP %s fully cut (all %d path(s); %s)", ip, paths, reason))
}

func (t alertTexts) flap(path string, n int) string {
	return t.pick(fmt.Sprintf("مسیر %s ناپایدار است: %d قطعی در یک ساعت", path, n), fmt.Sprintf("path %s flapping: %d cuts in the last hour", path, n))
}

func (t alertTexts) rttHigh(path string, v, base float64) string {
	return t.pick(fmt.Sprintf("تأخیر %s بالاست: %.0f ms (معمول %.0f ms)", path, v, base), fmt.Sprintf("RTT %s high: %.0f ms (usual %.0f ms)", path, v, base))
}

func (t alertTexts) rttWide(parts []string) string {
	return t.pick("تأخیر همه مسیرها با هم بالا رفته — مشکل از شبکه است، نه یک مسیر: "+strings.Join(parts, "، ")+" (ms فعلی/معمول)",
		"RTT rose on ALL paths together — network-wide, not one path: "+strings.Join(parts, ", ")+" (ms now/usual)")
}

func (t alertTexts) psi(v float64) string {
	return t.pick(fmt.Sprintf("فشار CPU بالاست: some avg60 = %.0f%%", v), fmt.Sprintf("CPU pressure high: some avg60 = %.0f%%", v))
}

func (t alertTexts) cert(days int) string {
	return t.pick(fmt.Sprintf("گواهی TLS تا %d روز دیگر منقضی می‌شود", days), fmt.Sprintf("TLS certificate expires in %d day(s)", days))
}

func (t alertTexts) probe(kind, e string) string {
	return t.pick(fmt.Sprintf("کاوش %s به پورت خودِ تونل ناموفق: %s", kind, e), fmt.Sprintf("%s probe to the tunnel's own port failed: %s", kind, e))
}

func (t alertTexts) frontFallback(path string, fallbacks, fails int64) string {
	return t.pick(fmt.Sprintf("مسیر %s از Reality به tls برگشت (%d بار بازگشت، %d شکست reality) — شاید Reality مسدود شده یا کلیدها ناهمخوان‌اند", path, fallbacks, fails),
		fmt.Sprintf("path %s fell back from reality to tls (%d fallbacks, %d reality failures) — reality blocked or a key mismatch?", path, fallbacks, fails))
}

func (t alertTexts) realityTarget(target, e string) string {
	return t.pick(fmt.Sprintf("هدف Reality %s در دسترس نیست: %s — کاوشگرها به‌جای سایت واقعی به بن‌بست می‌خورند", target, e),
		fmt.Sprintf("reality target %s is down: %s — probes get a dead end instead of the real site", target, e))
}

func (t alertTexts) test() string {
	return t.pick("پیام آزمایشی hs2 alert — ارسال کار می‌کند", "hs2 alert test message — delivery works")
}

func (t alertTexts) summary(d alertDay, worstH string, worstV float64, cuts []string, items ...string) string {
	c := strings.Join(cuts, " ")
	if c == "" {
		c = t.pick("هیچ", "none")
	}
	w := "-"
	if worstH != "" {
		w = fmt.Sprintf("%s:00 (%.0f ms)", worstH, worstV)
	}
	if len(items) == 0 {
		items = summaryItemNames
	}
	part := map[string]string{
		"users":    t.pick(fmt.Sprintf("بیشینه کاربر %d", d.PeakUsers), fmt.Sprintf("peak users %d", d.PeakUsers)),
		"mbit":     t.pick(fmt.Sprintf("بیشینه %.0f Mbit/s", d.PeakMbit), fmt.Sprintf("peak %.0f Mbit/s", d.PeakMbit)),
		"rtt":      t.pick("بدترین ساعت تأخیر "+w, "worst RTT hour "+w),
		"cuts":     t.pick("قطعی‌ها: "+c, "cuts: "+c),
		"restarts": t.pick(fmt.Sprintf("ری‌استارت %d", d.Restarts), fmt.Sprintf("restarts %d", d.Restarts)),
		"alerts":   t.pick(fmt.Sprintf("هشدار %d", d.Alerts), fmt.Sprintf("alerts %d", d.Alerts)),
	}
	var ps []string
	for _, it := range items {
		if p, ok := part[it]; ok {
			ps = append(ps, p)
		}
	}
	return t.pick("خلاصه روزانه: ", "daily summary: ") + strings.Join(ps, " · ")
}

// sevMark prefixes a message with its severity.
func sevMark(sev string) string {
	switch sev {
	case sevCrit:
		return "🔴"
	case sevWarn:
		return "🟠"
	case sevOK:
		return "✅"
	}
	return "ℹ️"
}

// formatAlert is the final line: mark, host/unit, text.
func formatAlert(host, unit string, m alertMsg) string {
	return fmt.Sprintf("%s [%s/%s] %s", sevMark(m.Sev), host, unit, m.Text)
}

// sortMsgs: critical first, then warnings, info, resolved.
func sortMsgs(ms []alertMsg) {
	rank := map[string]int{sevCrit: 0, sevWarn: 1, sevInfo: 2, sevOK: 3}
	sort.SliceStable(ms, func(i, j int) bool { return rank[ms[i].Sev] < rank[ms[j].Sev] })
}

// redact removes the bot token from any text (errors from net/http carry
// the full URL, which contains it).
func redact(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "***")
	return strings.ReplaceAll(s, url.PathEscape(token), "***")
}

// sendTelegram posts one message. The error never contains the token.
func sendTelegram(ctx context.Context, cfg alertCfg, text string) error {
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return nil
	}
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second}
	if cfg.Proxy != "" {
		pu, err := url.Parse(cfg.Proxy)
		if err != nil || pu.Host == "" {
			return fmt.Errorf("bad proxy URL")
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	cl := &http.Client{Transport: tr, Timeout: 20 * time.Second}
	api := strings.TrimRight(firstNonEmpty(cfg.APIURL, "https://api.telegram.org"), "/")
	form := url.Values{"chat_id": {cfg.ChatID}, "text": {text}, "disable_web_page_preview": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/bot"+cfg.BotToken+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("%s", redact(err.Error(), cfg.BotToken))
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("%s", redact(err.Error(), cfg.BotToken))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &r)
	if resp.StatusCode != 200 || !r.OK {
		return fmt.Errorf("telegram: HTTP %d %s", resp.StatusCode, redact(clip(r.Description, 120), cfg.BotToken))
	}
	return nil
}
