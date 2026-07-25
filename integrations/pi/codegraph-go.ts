// CodeGraph-Go Extension for Pi
// Local codegraph-go MCP bridge with safety rails for SSH / big homes.
//
// Design (no prompt hooks, no command enumeration):
// 1) /root 和 $HOME 可以当工作目录 —— cg-go home-mode 只索引「像项目」的一级目录
// 2) 每次工具结果有默认上限 + 硬封顶（模型可略调高，但不能炸会话）
// 3) 超长结果做温和压缩（保头尾），正常体量原样返回
// 4) 用户说需求即可，不必写 path/命令；由模型自己补技术参数。插件不猜口令、不钩消息。
//    （「人话」= 怎么说；「需求」= 说什么。两码事，别混。）
//
// Requires: codegraph-go on PATH

import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"
import { Type } from "typebox"
import { spawn, type ChildProcess, execFileSync } from "node:child_process"
import { createInterface } from "node:readline"
import fs from "node:fs"
import os from "node:os"
import path from "node:path"

const START_TIMEOUT_MS = Number(process.env.CODEGRAPH_GO_START_TIMEOUT_MS || 30000)
const REQUEST_TIMEOUT_MS = Number(process.env.CODEGRAPH_GO_REQUEST_TIMEOUT_MS || 120000)

// ---- Output budget ---------------------------------------------------------
// 目标：日常一次调用够用；真遇到超大结果才裁。
// cg-go 侧默认约 18k 字符；插件略低一点当二道闸，但别砍太狠。
// 与 Go 端 defaultOutputChars=18000 对齐，只在插件侧做二道闸，避免双重砍太狠。
const OUTPUT_CHAR_CAP = Number(process.env.CODEGRAPH_GO_OUTPUT_CHARS || 18000)
// 行数软上限：超过才头尾保留。正常搜索几十行不会触发。
const OUTPUT_LINE_CAP = Number(process.env.CODEGRAPH_GO_OUTPUT_LINES || 260)

// 默认条数与 Go 端一致；hard 明显高于 default。
const DEFAULT_SEARCH_MAX = Number(process.env.CODEGRAPH_GO_SEARCH_MAX || 70)
const HARD_SEARCH_MAX = Number(process.env.CODEGRAPH_GO_SEARCH_HARD || 120)
const DEFAULT_FILES_MAX = Number(process.env.CODEGRAPH_GO_FILES_MAX || 100)
const HARD_FILES_MAX = Number(process.env.CODEGRAPH_GO_FILES_HARD || 150)
const DEFAULT_SYMBOL_MAX = Number(process.env.CODEGRAPH_GO_SYMBOL_MAX || 40)
const HARD_SYMBOL_MAX = Number(process.env.CODEGRAPH_GO_SYMBOL_HARD || 80)
const DEFAULT_EXPLORE_MAX = Number(process.env.CODEGRAPH_GO_EXPLORE_MAX || 30)
const HARD_EXPLORE_MAX = Number(process.env.CODEGRAPH_GO_EXPLORE_HARD || 60)

const PROJECT_MARKERS = [
	".git",
	"go.mod",
	"package.json",
	"pyproject.toml",
	"Cargo.toml",
	"pom.xml",
	"build.gradle",
	"build.gradle.kts",
	"composer.json",
	"Gemfile",
	"mix.exs",
	"CMakeLists.txt",
	"Makefile",
	"Cargo.lock",
	"pnpm-workspace.yaml",
	"setup.py",
	"requirements.txt",
	"Pipfile",
	"build.sbt",
	"Package.swift",
]

function envFlag(name: string): boolean {
	const v = (process.env[name] || "").trim().toLowerCase()
	return v === "1" || v === "true" || v === "yes" || v === "on"
}

function clampInt(n: unknown, def: number, hard: number): number {
	let v = typeof n === "number" && Number.isFinite(n) ? Math.floor(n) : def
	if (v < 1) v = def
	if (v > hard) v = hard
	return v
}

function normPath(p: string): string {
	try {
		return fs.realpathSync.native(path.resolve(p))
	} catch {
		return path.resolve(p)
	}
}

function isFilesystemRoot(dir: string): boolean {
	const resolved = normPath(dir)
	if (resolved === path.parse(resolved).root) return true
	if (resolved === "/home" || resolved === "/Users") return true
	return false
}

function isHomeDir(dir: string): boolean {
	const home = normPath(os.homedir())
	const resolved = normPath(dir)
	return resolved === home || resolved === "/root"
}

function hasProjectMarker(dir: string): boolean {
	for (const m of PROJECT_MARKERS) {
		try {
			if (fs.existsSync(path.join(dir, m))) return true
		} catch {
			/* ignore */
		}
	}
	return false
}

function findGitRoot(start: string): string | null {
	try {
		const out = execFileSync("git", ["-C", start, "rev-parse", "--show-toplevel"], {
			encoding: "utf8",
			timeout: 3000,
			stdio: ["ignore", "pipe", "ignore"],
		}).trim()
		return out ? normPath(out) : null
	} catch {
		return null
	}
}

/** Walk up for a project marker; stop at home/root. */
function findMarkerRoot(start: string): string | null {
	const home = normPath(os.homedir())
	let cur = normPath(start)
	const root = path.parse(cur).root
	for (let i = 0; i < 24; i++) {
		if (cur === home || cur === root || cur === "/root") return null
		if (hasProjectMarker(cur)) return cur
		const parent = path.dirname(cur)
		if (parent === cur) return null
		cur = parent
	}
	return null
}

export type WorkdirDecision =
	| { ok: true; workdir: string; note: string }
	| { ok: false; reason: string }

/**
 * Pick indexing root.
 * - CODEGRAPH_GO_WORKDIR 优先
 * - 人在某个仓库里 → 用该仓库（更准）
 * - 人在 $HOME//root → 允许，cg-go home-mode 只扫项目目录
 * - 裸 /、/home、/Users → 拒绝（除非 ALLOW_BROAD）
 */
export function resolveWorkdir(cwd: string): WorkdirDecision {
	const allowBroad = envFlag("CODEGRAPH_GO_ALLOW_BROAD")
	const forced = (process.env.CODEGRAPH_GO_WORKDIR || "").trim()
	if (forced) {
		const w = normPath(forced)
		if (!allowBroad && isFilesystemRoot(w)) {
			return {
				ok: false,
				reason: `CODEGRAPH_GO_WORKDIR=${w} 是系统根目录。请换成用户目录或项目路径，或设 CODEGRAPH_GO_ALLOW_BROAD=1`,
			}
		}
		const note = isHomeDir(w)
			? "CODEGRAPH_GO_WORKDIR（家目录模式：只索引像项目的一级目录）"
			: "CODEGRAPH_GO_WORKDIR"
		return { ok: true, workdir: w, note }
	}

	const start = normPath(cwd)
	const gitRoot = findGitRoot(start)
	const markerRoot = findMarkerRoot(start)

	let candidate = start
	let note = "cwd"
	if (gitRoot && !isHomeDir(gitRoot) && !isFilesystemRoot(gitRoot)) {
		candidate = gitRoot
		note = "git 仓库根"
	} else if (markerRoot && !isHomeDir(markerRoot) && !isFilesystemRoot(markerRoot)) {
		candidate = markerRoot
		note = "项目标记目录"
	}

	if (!allowBroad && isFilesystemRoot(candidate)) {
		return {
			ok: false,
			reason: `拒绝 workdir=${candidate}（系统根）。请用家目录或项目路径。`,
		}
	}

	if (isHomeDir(candidate)) {
		note =
			note === "cwd"
				? "家目录模式（只索引带 go.mod/package.json/.git 等标记的一级目录）"
				: `${note} + 家目录模式`
	}

	return { ok: true, workdir: candidate, note }
}

/**
 * 温和压缩：只有真的超大才动刀。
 * - 先收多余空行
 * - 超行数：保留前 75% + 后 25%（中间说明省略了多少）
 * - 超字符：在行边界截断并提示
 * 小结果原样返回，避免「一次拿不齐」。
 */
function compressToolText(text: string): string {
	if (!text) return text

	let t = text.replace(/\r\n/g, "\n").replace(/[ \t]+$/gm, "")
	t = t.replace(/\n{4,}/g, "\n\n\n")

	const lineCap = OUTPUT_LINE_CAP > 0 ? OUTPUT_LINE_CAP : 220
	const lines = t.split("\n")
	let body: string
	if (lines.length > lineCap) {
		const headN = Math.max(40, Math.floor(lineCap * 0.75))
		const tailN = Math.max(15, lineCap - headN - 1)
		const omitted = lines.length - headN - tailN
		body =
			lines.slice(0, headN).join("\n") +
			`\n... (省略 ${omitted} 行；可再查得更具体，或略提高 max) ...\n` +
			lines.slice(-tailN).join("\n")
	} else {
		body = t
	}

	const cap = OUTPUT_CHAR_CAP > 0 ? OUTPUT_CHAR_CAP : 14000
	if (body.length <= cap) return body

	let truncAt = cap
	if (truncAt > 0 && (body.charCodeAt(truncAt - 1) & 0xfc00) === 0xd800) truncAt--
	const window = body.slice(0, truncAt)
	const nl = window.lastIndexOf("\n")
	const cut = nl > cap * 0.7 ? nl : truncAt
	return body.slice(0, cut) + `\n... (输出达上限 ${cap} 字，已截断；请缩小范围再查)`
}

/**
 * 对齐 Read 工具风格：把 CG 的 markdown 输出剥成纯文本。
 * - 去掉 ``` 围栏块（代码已由 CG 侧 skipCode/includeCode=false 控制）
 * - ## → 加个空行分段；**bold** → 去星号
 * - 列表 `- xxx` 保留（结构信息）
 * - rg fallback `file:line:content` → 只留 file:line
 * - 多余空行收一下
 */
function formatCleanText(text: string): string {
	if (!text) return text
	let t = text
	// 1) 剥代码块 → 行数摘要（兜底，CG 侧已控制不输出代码）
	t = t.replace(/```[\s\S]*?```/g, (block) => {
		const inner = block.slice(3, -3)
		const langEnd = inner.indexOf("\n")
		const code = langEnd > 0 ? inner.slice(langEnd + 1) : inner
		const lineCount = code.split("\n").filter((l) => l.trim().length > 0).length || 1
		return `[ ${lineCount} 行代码已省略 ]`
	})
	// 2) rg fallback: file:line:matched_content → file:line
	t = t.replace(/^([^:\n]+:\d+):.+$/gm, "$1")
	// 3) markdown 标题 → 纯文本（去 #，加空行）
	t = t.replace(/^#{1,6}\s+/gm, "")
	// 4) bold / italic → 去标记
	t = t.replace(/\*\*([^*]+)\*\*/g, "$1")
	t = t.replace(/\*([^*]+)\*/g, "$1")
	// 5) inline code → 去反引号
	t = t.replace(/`([^`]+)`/g, "$1")
	// 6) 去行尾空格 + 收多余空行
	t = t.replace(/[ \t]+$/gm, "")
	t = t.replace(/\n{4,}/g, "\n\n\n")
	// 7) 行首冒号/括号类的 markdown 残留清理
	t = t.replace(/^\s*>\s?/gm, "")
	return t.trim()
}

/** 补默认 max，并夹住硬上限。 */
function withToolDefaults(name: string, args: Record<string, unknown>): Record<string, unknown> {
	const out: Record<string, unknown> = { ...args }
	switch (name) {
		case "search":
			out.max_results = clampInt(out.max_results, DEFAULT_SEARCH_MAX, HARD_SEARCH_MAX)
			break
		case "files":
			out.max = clampInt(out.max, DEFAULT_FILES_MAX, HARD_FILES_MAX)
			break
		case "callers":
		case "callees":
		case "impact":
			out.max_results = clampInt(out.max_results, DEFAULT_SYMBOL_MAX, HARD_SYMBOL_MAX)
			break
		case "explore":
			// 0/缺省 = 服务端按仓库体量分档；不要强塞默认把 0 盖掉
			if (out.max !== undefined && out.max !== null && out.max !== 0) {
				out.max = clampInt(out.max, DEFAULT_EXPLORE_MAX, HARD_EXPLORE_MAX)
			} else {
				delete out.max
			}
			if (out.skipCode === undefined) out.skipCode = true
			break
		case "node":
			if (out.offset !== undefined && out.offset !== null) {
				out.offset = clampInt(out.offset, 1, 1_000_000)
			}
			if (out.limit !== undefined && out.limit !== null) {
				out.limit = clampInt(out.limit, 1, 2000)
			}
			if (out.includeCode === undefined) out.includeCode = false
			break
		case "affected":
			out.depth = clampInt(out.depth ?? 5, 5, 10)
			break
	}
	return out
}

interface MCPRequest {
	jsonrpc: "2.0"
	id: number
	method: string
	params?: Record<string, unknown>
}

interface MCPResponse {
	jsonrpc: "2.0"
	id: number
	result?: {
		content?: Array<{ type: string; text: string }>
		tools?: Array<{
			name: string
			description: string
			inputSchema: Record<string, unknown>
		}>
	}
	error?: {
		code: number
		message: string
	}
}

interface MCPToolResult {
	content: Array<{ type: string; text: string }>
	tools?: Array<{
		name: string
		description: string
		inputSchema: Record<string, unknown>
	}>
}

class CodeGraphClient {
	private proc: ChildProcess | null = null
	private requestId = 0
	private pending = new Map<
		number,
		{
			resolve: (result: MCPToolResult) => void
			reject: (err: Error) => void
			timer?: ReturnType<typeof setTimeout>
		}
	>()
	private tools: Array<{ name: string; description: string; inputSchema: Record<string, unknown> }> = []
	private initialized = false
	private starting: Promise<void> | null = null
	private stopped = false
	private restartAttempts = 0
	private restartTimer: ReturnType<typeof setTimeout> | null = null
	readonly workdir: string
	readonly workdirNote: string

	constructor(workdir: string, workdirNote = "cwd") {
		this.workdir = workdir
		this.workdirNote = workdirNote
	}

	async start(): Promise<void> {
		if (this.proc && this.initialized) return
		if (this.starting) return this.starting
		this.stopped = false
		this.starting = this.doStart().finally(() => {
			this.starting = null
		})
		return this.starting
	}

	private async doStart(): Promise<void> {
		if (this.proc) {
			try {
				this.proc.kill()
			} catch {
				/* ignore */
			}
			this.proc = null
		}

		const bin = process.env.CODEGRAPH_GO_BIN || "codegraph-go"
		this.proc = spawn(bin, ["-workdir", this.workdir], {
			stdio: ["pipe", "pipe", "pipe"],
		})

		this.proc.on("error", (err) => {
			console.error(`[codegraph-go] process error: ${err.message}`)
			this.cleanup()
			this.scheduleRestart()
		})

		this.proc.on("exit", (code) => {
			console.error(`[codegraph-go] process exited with code ${code}`)
			this.cleanup()
			this.scheduleRestart()
		})

		// Drain stderr so the child never blocks on a full pipe.
		if (this.proc.stderr) {
			this.proc.stderr.on("data", (chunk: Buffer | string) => {
				if (process.env.CODEGRAPH_GO_DEBUG) {
					const text = typeof chunk === "string" ? chunk : chunk.toString("utf8")
					console.error(`[codegraph-go] ${text.trimEnd()}`)
				}
			})
		}

		const rl = createInterface({ input: this.proc.stdout! })
		rl.on("line", (line) => {
			try {
				const msg: MCPResponse = JSON.parse(line)
				if (msg.id !== undefined && this.pending.has(msg.id)) {
					const entry = this.pending.get(msg.id)!
					this.pending.delete(msg.id)
					if (entry.timer) clearTimeout(entry.timer)
					if (msg.error) entry.reject(new Error(msg.error.message))
					else entry.resolve(msg.result as MCPToolResult)
				}
			} catch {
				// non-JSON noise
			}
		})

		await this.sendRequest(
			"initialize",
			{
				protocolVersion: "2024-11-05",
				capabilities: {},
				clientInfo: { name: "pi-codegraph-go", version: "0.4.0" },
			},
			START_TIMEOUT_MS,
		)
		this.sendNotification("notifications/initialized")

		const listResult = await this.sendRequest("tools/list", {}, START_TIMEOUT_MS)
		this.tools = listResult.tools || []
		this.initialized = true

		console.error(
			`[codegraph-go] started workdir=${this.workdir} (${this.workdirNote}), ` +
				`${this.tools.length} tools, out≤${OUTPUT_CHAR_CAP}c/${OUTPUT_LINE_CAP}L`,
		)
	}

	private sendRequest(
		method: string,
		params: Record<string, unknown>,
		timeoutMs: number = REQUEST_TIMEOUT_MS,
	): Promise<MCPToolResult> {
		return new Promise((resolve, reject) => {
			if (!this.proc?.stdin) {
				reject(new Error("codegraph-go not running"))
				return
			}
			const id = ++this.requestId
			const req: MCPRequest = { jsonrpc: "2.0", id, method, params }
			const timer = setTimeout(() => {
				if (!this.pending.has(id)) return
				this.pending.delete(id)
				reject(new Error(`codegraph-go ${method} timed out after ${timeoutMs}ms`))
			}, timeoutMs)
			this.pending.set(id, { resolve, reject, timer })
			this.proc.stdin.write(JSON.stringify(req) + "\n")
		})
	}

	private sendNotification(method: string, params?: Record<string, unknown>): void {
		if (!this.proc?.stdin) return
		this.proc.stdin.write(JSON.stringify({ jsonrpc: "2.0", method, params: params || {} }) + "\n")
	}

	async callTool(name: string, args: Record<string, unknown>): Promise<string> {
		if (!this.initialized) {
			// Auto-heal: process may have crashed; try one restart.
			await this.start()
		}
		if (!this.initialized) throw new Error("codegraph-go not initialized")
		const bounded = withToolDefaults(name, args)
		try {
			const result = await this.sendRequest("tools/call", { name, arguments: bounded })
			const text = result.content?.map((c) => c.text).join("\n") || "no result"
			this.restartAttempts = 0
			return formatCleanText(compressToolText(text))
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err)
			if (/not running|disconnected|timed out/i.test(msg)) {
				await this.start()
				const result = await this.sendRequest("tools/call", { name, arguments: bounded })
				const text = result.content?.map((c) => c.text).join("\n") || "no result"
				return formatCleanText(compressToolText(text))
			}
			throw err
		}
	}

	getTools() {
		return this.tools
	}

	/** Exponential backoff restart after unexpected exit (max ~5 tries). */
	private scheduleRestart() {
		if (this.stopped) return
		if (this.restartAttempts >= 5) {
			console.error("[codegraph-go] gave up auto-restart after 5 attempts")
			return
		}
		if (this.restartTimer) return
		const delay = Math.min(1000 * 2 ** this.restartAttempts, 15000)
		this.restartAttempts++
		console.error(`[codegraph-go] auto-restart in ${delay}ms (attempt ${this.restartAttempts})`)
		this.restartTimer = setTimeout(() => {
			this.restartTimer = null
			if (this.stopped) return
			this.start().catch((err) => {
				console.error(`[codegraph-go] auto-restart failed: ${err}`)
				this.scheduleRestart()
			})
		}, delay)
	}

	cleanup() {
		for (const { reject, timer } of this.pending.values()) {
			if (timer) clearTimeout(timer)
			reject(new Error("codegraph-go disconnected"))
		}
		this.pending.clear()
		this.proc = null
		this.initialized = false
	}

	stop() {
		this.stopped = true
		if (this.restartTimer) {
			clearTimeout(this.restartTimer)
			this.restartTimer = null
		}
		if (this.proc) {
			this.proc.stdin?.end()
			this.proc.kill()
			this.cleanup()
		}
	}
}

async function tryStartClient(
	cwd: string,
): Promise<{ client: CodeGraphClient } | { error: string }> {
	const decision = resolveWorkdir(cwd)
	if (!decision.ok) {
		console.error(`[codegraph-go] ${decision.reason}`)
		return { error: decision.reason }
	}
	try {
		const client = new CodeGraphClient(decision.workdir, decision.note)
		await client.start()
		return { client }
	} catch (err) {
		const msg = err instanceof Error ? err.message : String(err)
		console.error(`[codegraph-go] failed to start: ${msg}`)
		return { error: msg }
	}
}

/** 家目录下有哪些「像项目」的一级目录（仅提示用，不参与钩子）。 */
function listProjects(workdir: string): string[] {
	const out: string[] = []
	try {
		for (const ent of fs.readdirSync(workdir, { withFileTypes: true })) {
			if (!ent.isDirectory() || ent.name.startsWith(".")) continue
			if (ent.name === "go" || ent.name === "node_modules" || ent.name === "code_references") continue
			const full = path.join(workdir, ent.name)
			if (hasProjectMarker(full)) out.push(ent.name)
		}
	} catch {
		/* ignore */
	}
	return out.sort((a, b) => a.localeCompare(b))
}

export default function (pi: ExtensionAPI) {
	let client: CodeGraphClient | null = null
	let lastRefuseReason: string | null = null
	let toolsRegistered = false

	pi.on("session_start", async (_event, ctx) => {
		const result = await tryStartClient(ctx.cwd)
		if ("client" in result) {
			client = result.client
			lastRefuseReason = null
			if (!toolsRegistered) {
				registerTools(pi, () => client)
				toolsRegistered = true
			}
			const projects = isHomeDir(client.workdir) ? listProjects(client.workdir) : []
			const hint =
				projects.length > 0
					? ` | 项目: ${projects.slice(0, 10).join(", ")}${projects.length > 10 ? "…" : ""}`
					: ""
			ctx.ui.notify(`codegraph-go: ${client.workdir}（${client.workdirNote}）${hint}`, "info")
		} else {
			client = null
			lastRefuseReason = result.error
			ctx.ui.notify(`codegraph-go 未启动: ${result.error}`, "warning")
		}
	})

	pi.on("before_agent_start", async (event, _ctx) => {
		// 只追加说明，不解析用户口令、不改焦点、不拦截消息。
		if (client) {
			const home = isHomeDir(client.workdir)
			const projects = home ? listProjects(client.workdir) : []
			const projectLine = home
				? `家目录模式已开：索引里只有像项目的一级目录${projects.length ? `（${projects.join(", ")}）` : ""}。用户点名项目时代理自动补 path=，不依赖用户传。`
				: `当前索引根就是单个项目目录，一般不必再加 path。`

			return {
				systemPrompt:
					event.systemPrompt +
					`

## CodeGraph Tools

索引根: \`${client.workdir}\`（${client.workdirNote}）
${projectLine}
单次结果预算约 ${OUTPUT_CHAR_CAP} 字 / ${OUTPUT_LINE_CAP} 行（过大才压缩；正常体量不砍）。`,
			}
		}
		if (lastRefuseReason) {
			return {
				systemPrompt:
					event.systemPrompt +
					`

## CodeGraph Tools

本会话 CodeGraph 未启用: ${lastRefuseReason}
请用普通文件工具，或设置 CODEGRAPH_GO_WORKDIR。`,
			}
		}
	})

	pi.on("session_shutdown", async () => {
		if (client) {
			client.stop()
			client = null
		}
	})

	pi.registerCommand("codegraph-start", {
		description: "启动 codegraph-go",
		handler: async (_args, ctx) => {
			if (client) {
				ctx.ui.notify(`已在运行: ${client.workdir}（${client.workdirNote}）`, "info")
				return
			}
			const result = await tryStartClient(ctx.cwd)
			if ("client" in result) {
				client = result.client
				lastRefuseReason = null
				if (!toolsRegistered) {
					registerTools(pi, () => client)
					toolsRegistered = true
				}
				ctx.ui.notify(
					`已启动: ${client.workdir}（${client.workdirNote}），${client.getTools().length} 个工具`,
					"info",
				)
			} else {
				lastRefuseReason = result.error
				ctx.ui.notify(`启动失败: ${result.error}`, "error")
			}
		},
	})

	pi.registerCommand("codegraph-stop", {
		description: "停止 codegraph-go",
		handler: async (_args, ctx) => {
			if (!client) {
				ctx.ui.notify("codegraph-go 未运行", "info")
				return
			}
			client.stop()
			client = null
			ctx.ui.notify("已停止", "info")
		},
	})

	pi.registerCommand("codegraph-info", {
		description: "查看 codegraph 工作目录与上限",
		handler: async (_args, ctx) => {
			const decision = resolveWorkdir(ctx.cwd)
			const projects =
				decision.ok && isHomeDir(decision.workdir) ? listProjects(decision.workdir) : []
			const lines = [
				`cwd: ${ctx.cwd}`,
				`解析: ${decision.ok ? `${decision.workdir}（${decision.note}）` : `拒绝 — ${decision.reason}`}`,
				`运行中: ${client ? `${client.workdir}（${client.workdirNote}）` : "否"}`,
				`输出上限: ${OUTPUT_CHAR_CAP} 字 / ${OUTPUT_LINE_CAP} 行（仅超限才裁）`,
				`搜索默认/上限: ${DEFAULT_SEARCH_MAX}/${HARD_SEARCH_MAX}`,
				`文件列表默认/上限: ${DEFAULT_FILES_MAX}/${HARD_FILES_MAX}`,
				`符号默认/上限: ${DEFAULT_SYMBOL_MAX}/${HARD_SYMBOL_MAX}`,
				projects.length ? `家目录项目: ${projects.join(", ")}` : "",
				`WORKDIR 环境变量: ${process.env.CODEGRAPH_GO_WORKDIR || "（未设）"}`,
			].filter(Boolean)
			ctx.ui.notify(lines.join("\n"), "info")
		},
	})
}

function registerTools(pi: ExtensionAPI, getClient: () => CodeGraphClient | null) {
	const run = async (name: string, params: Record<string, unknown>) => {
		const c = getClient()
		if (!c) {
			return {
				content: [
					{
						type: "text" as const,
						text: "codegraph-go 未运行。可用 /codegraph-info 查看原因。",
					},
				],
				details: {},
			}
		}
		try {
			const result = await c.callTool(name, params)
			return { content: [{ type: "text" as const, text: result }], details: {} }
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err)
			return {
				content: [{ type: "text" as const, text: `codegraph error: ${msg}` }],
				details: {},
			}
		}
	}

	pi.registerTool({
		name: "codegraph_search",
		label: "CodeGraph Search",
		description: "按正则/文本搜代码，返回 file:line。可加 path/glob 收窄；结果有上限。",
		promptSnippet: "Search code by pattern with file:line results",
		promptGuidelines: [
			"Use codegraph_search for code search. It respects .gitignore and returns structured file:line results.",
			"Use codegraph_search when looking for function definitions, class declarations, string literals, or any code pattern.",
			"When the user names a project under a home workdir, pass path= that project yourself — do not ask the user for paths.",
		],
		parameters: Type.Object({
			pattern: Type.String({ description: "regex or literal pattern (ripgrep syntax)" }),
			path: Type.Optional(Type.String({ description: "optional subdirectory under workspace" })),
			glob: Type.Optional(Type.String({ description: 'optional file glob filter, e.g. "*.go"' })),
			max_results: Type.Optional(
				Type.Number({
					description: `global match cap (default ${DEFAULT_SEARCH_MAX}, hard max ${HARD_SEARCH_MAX})`,
				}),
			),
			ignore_case: Type.Optional(Type.Boolean({ description: "case-insensitive search" })),
		}),
		async execute(_id, params) {
			return run("search", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_files",
		label: "CodeGraph Files",
		description: "按 glob 列文件。结果有上限。",
		promptSnippet: "List files by glob pattern",
		promptGuidelines: [
			"Use codegraph_files for finding files. It respects .gitignore and supports glob patterns.",
		],
		parameters: Type.Object({
			pattern: Type.Optional(Type.String({ description: 'glob pattern, e.g. "src/**/*.go"' })),
			path: Type.Optional(Type.String({ description: "optional subdirectory under workspace" })),
			max: Type.Optional(
				Type.Number({ description: `cap (default ${DEFAULT_FILES_MAX}, hard max ${HARD_FILES_MAX})` }),
			),
		}),
		async execute(_id, params) {
			return run("files", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_explore",
		label: "CodeGraph Explore",
		description:
			"主工具。空 query=项目概览；query=符号名则一次返回位置+调用关系（默认不返回源码，要源码加 skipCode=false）。家目录模式可加 path=项目名。",
		promptSnippet: "Get project structure overview",
		promptGuidelines: [
			"When asked about a codebase or project, use codegraph_explore FIRST before doing anything else.",
			"For 'how does X work' pass query=X — explore returns locations + callers/callees (no source by default). Add skipCode: false to include source.",
			"In home mode, pass path=<project> to focus one repo.",
		],
		parameters: Type.Object({
			query: Type.Optional(
				Type.String({ description: "symbol or free-text; empty = overview" }),
			),
			path: Type.Optional(Type.String({ description: "optional project subdirectory (home mode)" })),
			max: Type.Optional(
				Type.Number({
					description: `cap on entries (default ${DEFAULT_EXPLORE_MAX}, hard max ${HARD_EXPLORE_MAX})`,
				}),
			),
			skipCode: Type.Optional(
				Type.Boolean({
					description: "omit source code from results (default true). Set false to include implementation bodies.",
				}),
			),
		}),
		async execute(_id, params) {
			return run("explore", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_callees",
		label: "CodeGraph Callees",
		description: "列出某符号调用了谁（优先调用图，缺边时才回退解析函数体）。",
		promptSnippet: "Find what a function calls",
		promptGuidelines: [
			"Use codegraph_callees to understand what a function depends on / calls. Graph-first; body-parse is fallback only.",
		],
		parameters: Type.Object({
			name: Type.String({ description: "symbol name to look for" }),
			file: Type.Optional(
				Type.String({ description: "pin definition to this file (path or basename) when overloaded" }),
			),
			path: Type.Optional(Type.String({ description: "optional subdirectory" })),
			glob: Type.Optional(Type.String({ description: 'optional file glob filter, e.g. "*.go"' })),
			max_results: Type.Optional(
				Type.Number({
					description: `cap (default ${DEFAULT_SYMBOL_MAX}, hard max ${HARD_SYMBOL_MAX})`,
				}),
			),
		}),
		async execute(_id, params) {
			return run("callees", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_callers",
		label: "CodeGraph Callers",
		description: "查找谁调用了某符号（优先调用图，缺边时才回退 ripgrep）。",
		promptSnippet: "Find all references to a symbol",
		promptGuidelines: [
			"Use codegraph_callers BEFORE renaming or modifying a symbol. Graph-first; results labeled if rg fallback.",
		],
		parameters: Type.Object({
			name: Type.String({ description: "symbol name to look for" }),
			file: Type.Optional(
				Type.String({ description: "pin definition to this file (path or basename) when overloaded" }),
			),
			path: Type.Optional(Type.String({ description: "optional subdirectory" })),
			glob: Type.Optional(Type.String({ description: 'optional file glob filter, e.g. "*.go"' })),
			max_results: Type.Optional(
				Type.Number({
					description: `cap (default ${DEFAULT_SYMBOL_MAX}, hard max ${HARD_SYMBOL_MAX})`,
				}),
			),
		}),
		async execute(_id, params) {
			return run("callers", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_impact",
		label: "CodeGraph Impact",
		description: "改某个符号的影响面（调用图 BFS；缺边时回退 rg 计数）。",
		promptSnippet: "Assess impact radius of changing a symbol",
		promptGuidelines: [
			"Use codegraph_impact before changing a symbol. Prefers graph blast-radius over plain text counts.",
		],
		parameters: Type.Object({
			name: Type.String({ description: "symbol name to look for" }),
			file: Type.Optional(
				Type.String({ description: "pin definition to this file (path or basename) when overloaded" }),
			),
			path: Type.Optional(Type.String({ description: "optional subdirectory" })),
			glob: Type.Optional(Type.String({ description: 'optional file glob filter, e.g. "*.go"' })),
			max_results: Type.Optional(
				Type.Number({
					description: `cap (default ${DEFAULT_SYMBOL_MAX}, hard max ${HARD_SYMBOL_MAX})`,
				}),
			),
		}),
		async execute(_id, params) {
			return run("impact", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_node",
		label: "CodeGraph Node",
		description:
			"双模式。(1) 只传 file = 像 Read 一样读整文件（带行号）+ 谁依赖它；(2) 传 name = 符号位置+签名+调用链（默认不返回源码，要源码加 includeCode=true）。重名一次返回全部。",
		promptSnippet: "Get symbol details with source, callers, callees",
		promptGuidelines: [
			"Use codegraph_node when you need the full picture of one symbol — location, signature, and who calls it / what it calls.",
			"By default, codegraph_node does NOT return source code. Add includeCode: true when you need to read the implementation.",
			"Prefer codegraph_node over separately calling callers + callees for the same symbol.",
			"Pass file alone (no name) instead of the Read tool for source files — numbered lines + dependents.",
			"For an overloaded name it returns every matching body in one call; pass file/line to pin one.",
		],
		parameters: Type.Object({
			name: Type.Optional(Type.String({ description: "symbol name (symbol mode). Omit and pass file alone to read a whole file." })),
			file: Type.Optional(
				Type.String({
					description:
						"file path or basename. Alone = file-read mode; with name = disambiguate overload",
				}),
			),
			line: Type.Optional(
				Type.Number({ description: "symbol mode: pin definition at/around this line" }),
			),
			includeCode: Type.Optional(
				Type.Boolean({ description: "symbol mode: include source body (default false). Set true to see implementation." }),
			),
			symbolsOnly: Type.Optional(
				Type.Boolean({ description: "file mode: symbol map + dependents only" }),
			),
			offset: Type.Optional(Type.Number({ description: "file mode: 1-based start line (like Read)" })),
			limit: Type.Optional(Type.Number({ description: "file mode: max lines (cap 2000)" })),
		}),
		async execute(_id, params) {
			return run("node", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_status",
		label: "CodeGraph Status",
		description: "索引健康状况：节点/边/文件数、待同步。",
		promptSnippet: "Check CodeGraph index health and stats",
		promptGuidelines: [
			"Use codegraph_status to verify the index is ready, see node/edge/file counts, or check if a specific file is indexed.",
			"If search/node return empty unexpectedly, check codegraph_status for pending sync or empty index.",
		],
		parameters: Type.Object({
			path: Type.Optional(Type.String({ description: "optional path to check specific file index status" })),
		}),
		async execute(_id, params) {
			return run("status", params as Record<string, unknown>)
		},
	})

	pi.registerTool({
		name: "codegraph_affected",
		label: "CodeGraph Affected",
		description: "根据改动的源文件，找出可能受影响的测试。",
		promptSnippet: "Find tests affected by changed source files",
		promptGuidelines: [
			"Use codegraph_affected after editing source files to decide which tests to run.",
			"Pass the list of changed source paths in files; optionally set depth or a custom test filter glob.",
		],
		parameters: Type.Object({
			files: Type.Array(Type.String(), { description: "list of changed source files" }),
			depth: Type.Optional(Type.Number({ description: "max dependency traversal depth (default 5, max 10)" })),
			filter: Type.Optional(Type.String({ description: "custom glob to identify test files" })),
		}),
		async execute(_id, params) {
			return run("affected", params as Record<string, unknown>)
		},
	})
}
