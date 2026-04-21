package api

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
)

type rootPageData struct {
	ServerURL       string
	TransferredText string
}

var rootPageTemplate = template.Must(template.New("root").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Syna server</title>
<style>
:root {
	color-scheme: dark;
	--bg: #070707;
	--panel: #111;
	--panel-strong: #171717;
	--text: #f6f6f6;
	--muted: #b9b9b9;
	--quiet: #777;
	--line: #2a2a2a;
	--code: #ededed;
}
* {
	box-sizing: border-box;
}
body {
	margin: 0;
	min-height: 100vh;
	background:
		radial-gradient(circle at 20% 0%, #2a2a2a 0, transparent 32rem),
		linear-gradient(145deg, #050505 0%, #111 52%, #050505 100%);
	color: var(--text);
	font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
}
main {
	width: min(980px, calc(100% - 32px));
	margin: 0 auto;
	padding: 56px 0;
}
.shell {
	display: grid;
	grid-template-columns: minmax(0, 1fr);
	gap: 24px;
	align-items: stretch;
}
.hero, .steps {
	min-width: 0;
	border: 1px solid var(--line);
	background: var(--panel);
	background: color-mix(in srgb, var(--panel) 92%, transparent);
	box-shadow: 0 24px 80px rgb(0 0 0 / 0.35);
}
.hero {
	min-height: 560px;
	padding: 36px;
	display: flex;
	flex-direction: column;
	justify-content: space-between;
}
.mark {
	width: 52px;
	height: 52px;
	border: 1px solid #4b4b4b;
	background: linear-gradient(145deg, #fff, #858585);
	color: #050505;
	display: grid;
	place-items: center;
	font-weight: 800;
	font-size: 24px;
}
.eyebrow {
	margin: 32px 0 14px;
	color: var(--muted);
	text-transform: uppercase;
	letter-spacing: 0.12em;
	font-size: 12px;
	font-weight: 700;
}
h1 {
	margin: 0;
	font-size: clamp(52px, 11vw, 116px);
	line-height: 0.9;
	letter-spacing: 0;
}
.hero-copy {
	display: grid;
	grid-template-columns: minmax(0, 1fr) max-content;
	gap: 30px;
	align-items: end;
}
.lede {
	margin: 24px 0 0;
	max-width: 610px;
	color: #dfdfdf;
	font-size: 20px;
	line-height: 1.55;
}
.meta {
	display: flex;
	gap: 10px;
	flex-wrap: wrap;
	margin-top: 38px;
}
.pill {
	border: 1px solid var(--line);
	background: #0b0b0b;
	color: var(--muted);
	padding: 9px 12px;
	font-size: 13px;
	text-decoration: none;
}
.pill:hover {
	color: var(--text);
	border-color: #555;
}
.transfer {
	justify-self: end;
	padding-bottom: 7px;
	text-align: right;
}
.transfer strong {
	display: block;
	color: #76f2a7;
	font-size: clamp(28px, 4vw, 44px);
	font-weight: 800;
	line-height: 1.1;
	white-space: nowrap;
	text-shadow: 0 0 22px rgb(118 242 167 / 0.16);
}
.transfer span {
	display: block;
	margin-top: 6px;
	color: var(--muted);
	font-size: 13px;
	font-weight: 700;
	letter-spacing: 0.08em;
	text-transform: uppercase;
}
.steps {
	padding: 28px;
	background: var(--panel-strong);
	background: color-mix(in srgb, var(--panel-strong) 94%, transparent);
}
.steps h2 {
	margin: 0 0 18px;
	font-size: 22px;
}
.step {
	display: grid;
	grid-template-columns: 34px minmax(0, 1fr);
	gap: 14px;
	padding: 18px 0;
	border-top: 1px solid var(--line);
}
.step > div:last-child {
	min-width: 0;
}
.step:first-of-type {
	border-top: 0;
	padding-top: 0;
}
.num {
	width: 34px;
	height: 34px;
	border: 1px solid #555;
	display: grid;
	place-items: center;
	color: #f4f4f4;
	font-size: 13px;
	font-weight: 700;
}
.step h3 {
	margin: 0 0 8px;
	font-size: 16px;
}
.step p {
	margin: 0;
	color: var(--muted);
	line-height: 1.5;
}
.command {
	display: grid;
	grid-template-columns: minmax(0, 1fr) auto;
	align-items: stretch;
	margin-top: 10px;
	border: 1px solid var(--line);
	background: #080808;
}
.command code {
	display: block;
	width: 100%;
	max-width: 100%;
	padding: 12px 13px;
	overflow-x: auto;
	white-space: nowrap;
	border: 0;
	color: var(--code);
	font: 13px/1.45 ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace;
}
.copy {
	width: 72px;
	border: 0;
	border-left: 1px solid var(--line);
	background: #101010;
	color: var(--muted);
	font: 700 12px/1 ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
	cursor: pointer;
}
.copy:hover, .copy:focus-visible {
	background: #181818;
	color: var(--text);
	outline: none;
}
.copy.copied {
	color: var(--text);
}
.foot {
	margin-top: 22px;
	color: var(--quiet);
	font-size: 13px;
	line-height: 1.45;
}
@media (max-width: 820px) {
	main {
		width: min(100% - 24px, 620px);
		padding: 24px 0;
	}
	.shell {
		gap: 14px;
	}
	.hero {
		min-height: auto;
		padding: 28px;
	}
		.lede {
			font-size: 18px;
		}
		.hero-copy {
			grid-template-columns: minmax(0, 1fr);
			gap: 22px;
		}
		.transfer {
			justify-self: start;
			padding-bottom: 0;
			text-align: left;
		}
		.steps {
			padding: 22px;
		}
	.command {
		grid-template-columns: minmax(0, 1fr);
	}
	.copy {
		width: 100%;
		min-height: 42px;
		border-left: 0;
		border-top: 1px solid var(--line);
	}
}
</style>
</head>
<body>
<main>
	<div class="shell">
		<section class="hero" aria-labelledby="title">
			<div>
				<div class="mark" aria-hidden="true">S</div>
				<p class="eyebrow">Server online</p>
				<div class="hero-copy">
					<div>
						<h1 id="title">Syna</h1>
						<p class="lede">Private folder sync for your Linux devices. This server relays encrypted metadata and object blobs; your workspace key stays on your clients.</p>
					</div>
					<div class="transfer" aria-label="Server transfer statistics">
						<strong>{{.TransferredText}}</strong>
						<span>transferred to devices</span>
					</div>
				</div>
			</div>
			<nav class="meta" aria-label="Server links">
				<a class="pill" href="/readyz">Readiness</a>
			</nav>
		</section>

		<section class="steps" aria-labelledby="next">
			<h2 id="next">What to do next</h2>
			<div class="step">
				<div class="num">1</div>
				<div>
					<h3>Install the client</h3>
					<p>Run this on your first Linux device.</p>
					<div class="command">
						<code>curl -fsSL https://raw.githubusercontent.com/trckster/syna/master/scripts/install.sh | sh</code>
						<button class="copy" type="button" data-copy="curl -fsSL https://raw.githubusercontent.com/trckster/syna/master/scripts/install.sh | sh">Copy</button>
					</div>
				</div>
			</div>
			<div class="step">
				<div class="num">2</div>
				<div>
					<h3>Connect to this server</h3>
					<p>Use the same public URL your devices will reach.</p>
					<div class="command">
						<code>syna connect {{.ServerURL}}</code>
						<button class="copy" type="button" data-copy="syna connect {{.ServerURL}}">Copy</button>
					</div>
				</div>
			</div>
			<div class="step">
				<div class="num">3</div>
				<div>
					<h3>Add a folder</h3>
					<p>Choose the local file or directory you want Syna to keep in sync.</p>
					<div class="command">
						<code>syna add "$HOME/Documents"</code>
						<button class="copy" type="button" data-copy='syna add "$HOME/Documents"'>Copy</button>
					</div>
				</div>
			</div>
			<div class="step">
				<div class="num">4</div>
				<div>
					<h3>Join other devices</h3>
					<p>Run <strong>syna connect</strong> on another Linux device and enter the recovery key from the first one.</p>
				</div>
			</div>
			<p class="foot">Keep the server behind HTTPS with persistent storage mounted at /var/lib/syna.</p>
		</section>
	</div>
</main>
<script>
const copyButtons = document.querySelectorAll(".copy");

async function copyText(text) {
	if (navigator.clipboard && window.isSecureContext) {
		await navigator.clipboard.writeText(text);
		return;
	}
	const input = document.createElement("textarea");
	input.value = text;
	input.setAttribute("readonly", "");
	input.style.position = "fixed";
	input.style.opacity = "0";
	document.body.appendChild(input);
	input.select();
	document.execCommand("copy");
	input.remove();
}

for (const button of copyButtons) {
	button.addEventListener("click", async () => {
		try {
			await copyText(button.dataset.copy || "");
			button.textContent = "Copied";
			button.classList.add("copied");
			window.setTimeout(() => {
				button.textContent = "Copy";
				button.classList.remove("copied");
			}, 1600);
		} catch {
			button.textContent = "Failed";
			window.setTimeout(() => {
				button.textContent = "Copy";
			}, 1600);
		}
	});
}
</script>
</body>
</html>`))

func (a *API) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	transferred, err := a.db.TransferredBytes()
	if err != nil && a.logger != nil {
		a.logger.Printf("load transferred bytes: %v", err)
	}
	if err := rootPageTemplate.Execute(w, rootPageData{
		ServerURL:       a.rootServerURL(r),
		TransferredText: formatTransferredMB(transferred),
	}); err != nil && a.logger != nil {
		a.logger.Printf("render root page: %v", err)
	}
}

func (a *API) rootServerURL(r *http.Request) string {
	if a.cfg.PublicBaseURL != "" {
		return strings.TrimRight(a.cfg.PublicBaseURL, "/")
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	if r.Host == "" {
		return "https://your-syna-server.example"
	}
	return scheme + "://" + r.Host
}

func formatTransferredMB(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	return fmt.Sprintf("%.2f MB", float64(bytes)/1_000_000)
}
