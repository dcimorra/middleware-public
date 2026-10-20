package security

import (
	"sort"
	"strings"
)

// What a User-Agent says is the only thing a client tells about itself
// unasked, and it can say anything. Nobody can know which browser is
// really calling; what is true is that nearly all automated traffic says
// what it is, serious crawlers on purpose so they can be kept out, and
// that is enough for what this door is asked to do.

// Option is one catalogue entry: the key a manifest stores and how it
// reads. The panel translates the names through its own catalogue.
type Option struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

// browsers are asked in this order, not alphabetically: Edge claims to be
// Chrome, Chrome claims to be Safari, Safari claims to be Mozilla. The
// first match wins, so the ones in disguise come before who they dress as.
var browsers = []struct {
	key, name string
	marks     []string
}{
	{"edge", "Edge", []string{"edg/", "edga/", "edgios/", "edge/"}},
	{"opera", "Opera", []string{"opr/", "opios/", "opera"}},
	{"samsung", "Samsung Internet", []string{"samsungbrowser"}},
	{"ie", "Internet Explorer", []string{"msie ", "trident/"}},
	{"firefox", "Firefox", []string{"firefox/", "fxios/"}},
	{"chrome", "Chrome", []string{"chrome/", "crios/", "chromium/"}},
	{"safari", "Safari", []string{"safari/"}},
}

// BrowserOther is every User-Agent that is none of the known browsers,
// robots included, which is what makes an allow list of browsers worth
// anything. BrowserNone is a request that sends no User-Agent at all.
const (
	BrowserOther = "other"
	BrowserNone  = "none"
)

// Browser is the family a lower-cased User-Agent belongs to.
func Browser(ua string) string {
	if strings.TrimSpace(ua) == "" {
		return BrowserNone
	}
	for _, b := range browsers {
		for _, m := range b.marks {
			if strings.Contains(ua, m) {
				return b.key
			}
		}
	}
	return BrowserOther
}

// Browsers is the catalogue, so the panel does not repeat it.
func Browsers() []Option {
	out := make([]Option, 0, len(browsers)+2)
	for _, b := range browsers {
		out = append(out, Option{Key: b.key, Name: b.name})
	}
	return append(out,
		Option{Key: BrowserOther, Name: "Other", Note: "anything not listed above: robots, programs, rare browsers"},
		Option{Key: BrowserNone, Name: "None", Note: "sends no User-Agent"},
	)
}

func isBrowser(key string) bool {
	if key == BrowserOther || key == BrowserNone {
		return true
	}
	for _, b := range browsers {
		if b.key == key {
			return true
		}
	}
	return false
}

// Robots are closed by family, not by name: what gets decided is "no AI
// crawlers" or "no SEO crawlers", and new names appear every month.
var families = []Option{
	{"search", "Search engines", "Google, Bing, DuckDuckGo. Closing them takes the site out of search results"},
	{"ai", "AI crawlers", "the ones that take the text to train or to answer: GPTBot, ClaudeBot, CCBot"},
	{"social", "Social networks", "the ones that fetch the card when a link is pasted. Closing them leaves links without a preview"},
	{"seo", "SEO and commercial crawlers", "Ahrefs, Semrush and the like. They give the site nothing and ask the most"},
	{"tools", "Programs and libraries", "curl, wget, python-requests, headless browsers. Deploy scripts use them too"},
	{"monitors", "Outside monitoring", "UptimeRobot, Pingdom and the like. Whoever watches this site from outside is shut out"},
	{"other", "Unidentified robots", "anything that says bot, crawler or spider and is in no table; next month's new names land here"},
}

// Families is the catalogue of robot families.
func Families() []Option { return append([]Option(nil), families...) }

func isFamily(key string) bool {
	for _, f := range families {
		if f.Key == key {
			return true
		}
	}
	return false
}

// Robot is a recognised robot.
type Robot struct {
	Name   string `json:"name"`
	Family string `json:"family"`
}

// Robots is the whole table, so the panel can show who falls in each
// family: a family with no examples says nothing.
func Robots() []Robot {
	out := make([]Robot, 0, len(robots))
	for _, b := range robots {
		out = append(out, Robot{Name: b.name, Family: b.family})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// robots: the lower-case mark looked for in the User-Agent, and whose it
// is. Sorted once, longest mark first (init).
var robots = []struct{ mark, name, family string }{
	{"googlebot", "Googlebot", "search"},
	{"google-inspectiontool", "Google Inspection Tool", "search"},
	{"storebot-google", "Google StoreBot", "search"},
	{"bingbot", "Bingbot", "search"},
	{"bingpreview", "Bing Preview", "search"},
	{"msnbot", "MSNBot", "search"},
	{"applebot", "Applebot", "search"},
	{"duckduckbot", "DuckDuckBot", "search"},
	{"duckduckgo-favicons", "DuckDuckGo Favicons", "search"},
	{"yandexbot", "YandexBot", "search"},
	{"yandeximages", "YandexImages", "search"},
	{"baiduspider", "Baiduspider", "search"},
	{"slurp", "Yahoo Slurp", "search"},
	{"sogou", "Sogou", "search"},
	{"seznambot", "SeznamBot", "search"},
	{"qwantify", "Qwant", "search"},
	{"mojeekbot", "MojeekBot", "search"},
	{"naver", "Naver", "search"},
	{"petalbot", "PetalBot", "search"},
	{"exabot", "Exabot", "search"},

	{"gptbot", "GPTBot", "ai"},
	{"oai-searchbot", "OAI-SearchBot", "ai"},
	{"chatgpt-user", "ChatGPT-User", "ai"},
	{"claudebot", "ClaudeBot", "ai"},
	{"claude-web", "Claude-Web", "ai"},
	{"claude-user", "Claude-User", "ai"},
	{"claude-searchbot", "Claude-SearchBot", "ai"},
	{"anthropic-ai", "anthropic-ai", "ai"},
	{"ccbot", "CCBot", "ai"},
	{"perplexitybot", "PerplexityBot", "ai"},
	{"perplexity-user", "Perplexity-User", "ai"},
	{"google-extended", "Google-Extended", "ai"},
	{"applebot-extended", "Applebot-Extended", "ai"},
	{"bytespider", "Bytespider", "ai"},
	{"amazonbot", "Amazonbot", "ai"},
	{"meta-externalagent", "Meta-ExternalAgent", "ai"},
	{"meta-externalfetcher", "Meta-ExternalFetcher", "ai"},
	{"facebookbot", "FacebookBot", "ai"},
	{"cohere-ai", "cohere-ai", "ai"},
	{"cohere-training-data-crawler", "Cohere training crawler", "ai"},
	{"diffbot", "Diffbot", "ai"},
	{"imagesiftbot", "ImagesiftBot", "ai"},
	{"omgili", "Omgili", "ai"},
	{"timpibot", "Timpibot", "ai"},
	{"youbot", "YouBot", "ai"},
	{"ai2bot", "AI2Bot", "ai"},
	{"mistralai-user", "MistralAI-User", "ai"},
	{"duckassistbot", "DuckAssistBot", "ai"},
	{"pangubot", "PanguBot", "ai"},
	{"webzio-extended", "Webz.io", "ai"},

	{"facebookexternalhit", "facebookexternalhit", "social"},
	{"twitterbot", "Twitterbot", "social"},
	{"linkedinbot", "LinkedInBot", "social"},
	{"whatsapp", "WhatsApp", "social"},
	{"telegrambot", "TelegramBot", "social"},
	{"slackbot", "Slackbot", "social"},
	{"discordbot", "Discordbot", "social"},
	{"pinterest", "Pinterest", "social"},
	{"redditbot", "redditbot", "social"},
	{"embedly", "Embedly", "social"},
	{"skypeuripreview", "Skype", "social"},
	{"vkshare", "VK", "social"},
	{"mastodon", "Mastodon", "social"},
	{"snapchat", "Snapchat", "social"},
	{"iframely", "Iframely", "social"},
	{"tiktok", "TikTok", "social"},

	{"ahrefsbot", "AhrefsBot", "seo"},
	{"semrushbot", "SemrushBot", "seo"},
	{"mj12bot", "MJ12bot", "seo"},
	{"dotbot", "DotBot", "seo"},
	{"dataforseobot", "DataForSeoBot", "seo"},
	{"blexbot", "BLEXBot", "seo"},
	{"barkrowler", "Barkrowler", "seo"},
	{"screaming frog", "Screaming Frog", "seo"},
	{"seokicks", "SEOkicks", "seo"},
	{"sistrix", "SISTRIX", "seo"},
	{"serpstatbot", "serpstatbot", "seo"},
	{"zoominfobot", "ZoominfoBot", "seo"},
	{"linkdexbot", "Linkdex", "seo"},
	{"rogerbot", "rogerbot", "seo"},

	{"curl/", "curl", "tools"},
	{"wget", "Wget", "tools"},
	{"python-requests", "python-requests", "tools"},
	{"python-urllib", "urllib", "tools"},
	{"aiohttp", "aiohttp", "tools"},
	{"httpx", "httpx", "tools"},
	{"go-http-client", "Go-http-client", "tools"},
	{"java/", "Java", "tools"},
	{"okhttp", "okhttp", "tools"},
	{"axios", "axios", "tools"},
	{"node-fetch", "node-fetch", "tools"},
	{"got (", "got", "tools"},
	{"guzzlehttp", "Guzzle", "tools"},
	{"libwww-perl", "libwww-perl", "tools"},
	{"apache-httpclient", "Apache HttpClient", "tools"},
	{"postmanruntime", "Postman", "tools"},
	{"insomnia", "Insomnia", "tools"},
	{"scrapy", "Scrapy", "tools"},
	{"headlesschrome", "Headless Chrome", "tools"},
	{"phantomjs", "PhantomJS", "tools"},
	{"selenium", "Selenium", "tools"},
	{"playwright", "Playwright", "tools"},
	{"puppeteer", "Puppeteer", "tools"},
	{"nutch", "Nutch", "tools"},
	{"zgrab", "zgrab", "tools"},
	{"masscan", "masscan", "tools"},
	{"nmap", "Nmap", "tools"},
	{"winhttp", "WinHTTP", "tools"},

	{"uptimerobot", "UptimeRobot", "monitors"},
	{"pingdom", "Pingdom", "monitors"},
	{"statuscake", "StatusCake", "monitors"},
	{"betteruptime", "Better Uptime", "monitors"},
	{"site24x7", "Site24x7", "monitors"},
	{"newrelicpinger", "New Relic", "monitors"},
	{"gtmetrix", "GTmetrix", "monitors"},
	{"chrome-lighthouse", "Lighthouse", "monitors"},
	{"uptime-kuma", "Uptime Kuma", "monitors"},
	{"hetrixtool", "HetrixTools", "monitors"},
	{"datadog", "Datadog", "monitors"},
	{"zabbix", "Zabbix", "monitors"},
	{"checkly", "Checkly", "monitors"},
	{"freshping", "Freshping", "monitors"},
}

func init() {
	// Longest mark first: "applebot-extended" must be asked before
	// "applebot", or Apple's AI crawler passes as its search engine and
	// gets in where it was shut out.
	sort.SliceStable(robots, func(i, j int) bool { return len(robots[i].mark) > len(robots[j].mark) })
}

// robotMarks are what an unknown robot still says about itself.
var robotMarks = []string{"bot/", "bot ", "bot;", "crawler", "spider", "+http"}

// Bot says whether a lower-cased User-Agent is a robot, and whose. One
// that says "bot", "crawler" or "spider" and is in no table is a robot
// all the same, in family "other": that is how closing the robots still
// works a year from now without anyone touching this file.
func Bot(ua string) (Robot, bool) {
	if ua == "" {
		return Robot{}, false
	}
	for _, b := range robots {
		if strings.Contains(ua, b.mark) {
			return Robot{Name: b.name, Family: b.family}, true
		}
	}
	for _, m := range robotMarks {
		if strings.Contains(ua, m) {
			return Robot{Name: "unidentified robot", Family: "other"}, true
		}
	}
	return Robot{}, false
}
