package tlscarrier

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

// NginxErrorPage is the body nginx (server_tokens off) sends for an error
// status: its built-in page, CRLF line ends, the "nginx" tail. detail, when
// not "", is the extra centre line nginx adds (e.g. on a plain-HTTP request
// to an HTTPS port, whose title also carries it).
func NginxErrorPage(code int, detail string) string {
	reason := nginxReason(code)
	title := fmt.Sprintf("%d %s", code, reason)
	if detail != "" {
		title = fmt.Sprintf("%d %s", code, detail)
	}
	var b strings.Builder
	b.WriteString("<html>\r\n<head><title>" + title + "</title></head>\r\n<body>\r\n")
	b.WriteString(fmt.Sprintf("<center><h1>%d %s</h1></center>\r\n", code, reason))
	if detail != "" {
		b.WriteString("<center>" + detail + "</center>\r\n")
	}
	b.WriteString("<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n")
	return b.String()
}

// NginxError is a whole nginx error response in nginx's header order. close
// adds "Connection: close" (nginx closes after a 400), else keep-alive.
func NginxError(code int, detail string, now time.Time, close bool) string {
	body := NginxErrorPage(code, detail)
	conn := "keep-alive"
	if close {
		conn = "close"
	}
	return fmt.Sprintf("HTTP/1.1 %d %s\r\nServer: nginx\r\nDate: %s\r\nContent-Type: text/html\r\nContent-Length: %d\r\nConnection: %s\r\n\r\n%s",
		code, nginxReason(code), now.UTC().Format(http.TimeFormat), len(body), conn, body)
}

// nginxReason is nginx's reason phrase (it differs from Go's for some codes).
func nginxReason(code int) string {
	switch code {
	case 405:
		return "Not Allowed"
	case 413:
		return "Request Entity Too Large"
	case 414:
		return "Request-URI Too Large"
	case 416:
		return "Requested Range Not Satisfiable"
	}
	return http.StatusText(code)
}
