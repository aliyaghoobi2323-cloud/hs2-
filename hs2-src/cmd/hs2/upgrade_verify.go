package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// sec370 (sec353 1h, C1 interim): `tunnel renew --upgrade-from SRC` used to
// EXECUTE src (`src version`, as root) before any integrity check. Now src is
// read once, its sha256 is checked against the expected value, and only the
// verified bytes are staged and run; src itself is never executed, so a file
// swapped after the check cannot run either. Fail closed: a mismatch, or no
// expected hash at all, refuses the upgrade. The expected hash comes from
// --sha256 HEX, else from SRC.sha256 beside it (the release's
// hs2-linux-amd64.sha256 format: the first 64-hex token) — only when both
// files are root-owned on a local disk filesystem in a directory that, with
// every ancestor, others cannot write (b376, b378: sidecarTrust). Signatures (sec353
// phase 3) will slot in here.
//
// HS2_ALLOW_UNVERIFIED=1 (the install.sh escape hatch, same name) installs a
// binary that has no expected hash, printing its full sha256; a mismatch is
// refused even then.

var hex64 = regexp.MustCompile(`(?i)[0-9a-f]{64}`)

// parseSha256 pulls the first 64-hex token (any case, CRLF or LF) and
// lower-cases it, "" when there is none.
func parseSha256(s string) string {
	return strings.ToLower(hex64.FindString(s))
}

// trustedOwner: root, or the user running hs2 (root in production; the
// tests' user otherwise).
func trustedOwner(fi os.FileInfo) bool {
	uid, ok := fileUID(fi)
	return ok && (uid == 0 || int(uid) == os.Geteuid())
}

// sidecarTrust: see trust_linux.go (b376, b378).

// upgradeWantHash: the expected sha256 for src (flag first, then a trusted
// src.sha256), and where it came from; "" when there is none. untrusted is
// set when src.sha256 could not be trusted (see sidecarTrust).
func upgradeWantHash(src string, srcFI os.FileInfo, flagHash string) (want, from, untrusted string, err error) {
	if f := strings.TrimSpace(flagHash); f != "" {
		h := parseSha256(f)
		if h == "" || len(f) != 64 {
			return "", "", "", fmt.Errorf("--sha256 %q is not a 64-hex sha256", f)
		}
		return h, "--sha256", "", nil
	}
	body, why, _ := sidecarTrust(src, srcFI)
	if why != "" {
		return "", "", why, nil
	}
	if body == "" {
		return "", "", "", nil
	}
	h := parseSha256(body)
	if h == "" {
		return "", "", "", fmt.Errorf("%s.sha256 has no sha256 in it", src)
	}
	return h, src + ".sha256", "", nil
}

// stageVerifiedBinary reads src once, verifies it, writes the verified bytes
// to dst (mode 0755, replacing a stale dst) and only then runs `dst version`.
// It returns the version line. On any failure dst is removed and src was never
// executed.
func stageVerifiedBinary(src, flagHash, dst string, allowUnverified bool) (string, error) {
	sf, err := os.Open(src)
	if err != nil {
		return "", err
	}
	srcFI, err := sf.Stat()
	if err != nil {
		sf.Close()
		return "", err
	}
	b, err := io.ReadAll(sf) // the bytes hashed, staged and run: read once, from this fd
	sf.Close()
	if err != nil {
		return "", err
	}
	want, from, untrusted, err := upgradeWantHash(src, srcFI, flagHash)
	if err != nil {
		return "", fmt.Errorf(tr("%v — not installed", "%v — نصب نشد"), err)
	}
	if untrusted != "" && !allowUnverified {
		return "", fmt.Errorf(tr("%s.sha256 is not trusted (%s): anyone who can write there could swap both files — pass --sha256 HEX from the release or the kharej server — NOT installed",
			"%s.sha256 قابل اعتماد نیست (%s): هر کس آنجا بنویسد می‌تواند هر دو فایل را عوض کند — --sha256 HEX را از release یا سرور خارج بدهید — نصب نشد"), src, untrusted)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])
	switch {
	case want != "" && got != want:
		return "", fmt.Errorf(tr("sha256 mismatch: %s says %s, %s is %s — NOT installed (copy both files again)",
			"sha256 نمی‌خواند: %s می‌گوید %s ولی %s برابر %s است — نصب نشد (هر دو فایل را دوباره کپی کنید)"), from, want, src, got)
	case want == "" && !allowUnverified:
		return "", fmt.Errorf(tr("no expected sha256 for %s: no %s.sha256 found — copy it beside it (from the release or the kharej server) or pass --sha256 HEX; its sha256 is %s — NOT installed (HS2_ALLOW_UNVERIFIED=1 installs it anyway)",
			"sha256 مورد انتظار برای %s نیست: %s.sha256 پیدا نشد — آن را کنارش کپی کنید (از release یا سرور خارج) یا --sha256 HEX بدهید؛ sha256 آن %s است — نصب نشد (HS2_ALLOW_UNVERIFIED=1 بدون بررسی نصب می‌کند)"), src, src, got)
	case want == "":
		fmt.Println(tr("WARN: installing an UNVERIFIED binary (HS2_ALLOW_UNVERIFIED=1), sha256 ", "هشدار: نصب باینری بررسی‌نشده (HS2_ALLOW_UNVERIFIED=1)، sha256 ") + got)
	default:
		fmt.Println(tr("sha256 matches ", "sha256 می‌خواند با ") + from + ": " + got)
	}
	os.Remove(dst)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(dst)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(dst)
		return "", err
	}
	out, err := exec.Command(dst, "version").Output()
	if err != nil || !strings.Contains(string(out), "hs2") {
		os.Remove(dst)
		return "", fmt.Errorf("%s does not run as hs2 (%v) — not installed", src, err)
	}
	return strings.TrimSpace(string(out)), nil
}
