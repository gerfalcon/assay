package verdict

import "strings"

// OrgFromEmail infers an organisation from an email domain.
//
// A FALLBACK, never the primary source. An explicit org always wins, because a
// great many engineers commit from gmail, from a GitHub noreply address, or from
// a personal domain — and inferring "gmail" as an organisation would corrupt the
// one criterion that stops a rule being promoted on a single team's house style.
//
// So: if the domain looks like a consumer mail provider, return nothing and let
// the caller fall back to an explicit value or to no attribution at all. No
// attribution is an honest answer; a wrong one is not.
func OrgFromEmail(email string) string {
	at := strings.LastIndex(email, "@")
	// at == 0 means no local part, which is not an address anyone owns.
	if at <= 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if domain == "" || consumerDomains[domain] {
		return ""
	}
	// GitHub noreply addresses are per-user, not per-org: 1234+name@users.noreply.github.com
	if strings.HasSuffix(domain, "users.noreply.github.com") {
		return ""
	}
	// Strip a public suffix so acme.co.uk and acme.com read as the same org.
	// Not a full PSL implementation — that would be a dependency for a
	// heuristic, and this is already the fallback path.
	parts := strings.Split(domain, ".")
	if len(parts) >= 3 && secondLevel[parts[len(parts)-2]+"."+parts[len(parts)-1]] {
		return parts[len(parts)-3]
	}
	if len(parts) >= 2 {
		return parts[len(parts)-2]
	}
	return domain
}

// consumerDomains are providers that say nothing about who someone works for.
var consumerDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true,
	"hotmail.com": true, "hotmail.co.uk": true, "live.com": true,
	"yahoo.com": true, "yahoo.co.uk": true, "ymail.com": true,
	"icloud.com": true, "me.com": true, "mac.com": true,
	"proton.me": true, "protonmail.com": true, "pm.me": true,
	"fastmail.com": true, "fastmail.fm": true,
	"gmx.com": true, "gmx.de": true, "gmx.net": true, "web.de": true,
	"aol.com": true, "mail.com": true, "zoho.com": true,
	"yandex.com": true, "yandex.ru": true, "tutanota.com": true,
	"hey.com": true, "duck.com": true, "qq.com": true, "163.com": true,
	"posteo.de": true, "mailbox.org": true,
	"example.com": true, "localhost": true,
}

// secondLevel are public suffixes where the org name is one label further left.
var secondLevel = map[string]bool{
	"co.uk": true, "org.uk": true, "ac.uk": true, "gov.uk": true,
	"com.au": true, "net.au": true, "org.au": true,
	"co.nz": true, "co.za": true, "co.jp": true, "or.jp": true,
	"com.br": true, "com.mx": true, "com.sg": true, "com.tr": true,
	"co.in": true, "co.il": true, "com.cn": true,
}
