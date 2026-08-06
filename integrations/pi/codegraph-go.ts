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
// 非 DEBUG 时保留最近多少行 stderr，进程退出/重启时打出来排查（仍照常 drain 防管道阻塞）。
const STDERR_KEEP_LINES = 20

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

/** 补默认 max，并夹住硬上限。
 * v0.8+ 是单个折叠工具 codegraph（action=…）：按 args.action 分发，
 * 不能再按旧工具名 switch（callTool 固定走 "codegraph"）。
 */
function withToolDefaults(args: Record<string, unknown>): Record<string, unknown> {
	const out: Record<string, unknown> = { ...args }
	const action = typeof out.action === "string" ? out.action : ""
	switch (action) {
		case "search":
			// max_results 与 max 二选一：谁传了夹谁；都没传补默认。
			if (out.max_results !== undefined && out.max_results !== null) {
				out.max_results = clampInt(out.max_results, DEFAULT_SEARCH_MAX, HARD_SEARCH_MAX)
			} else if (out.max !== undefined && out.max !== null) {
				out.max = clampInt(out.max, DEFAULT_SEARCH_MAX, HARD_SEARCH_MAX)
			} else {
				out.max_results = DEFAULT_SEARCH_MAX
			}
			break
		case "files":
			out.max = clampInt(out.max, DEFAULT_FILES_MAX, HARD_FILES_MAX)
			break
		case "callers":
		case "callees":
		case "impact":
			if (out.max_results !== undefined && out.max_results !== null) {
				out.max_results = clampInt(out.max_results, DEFAULT_SYMBOL_MAX, HARD_SYMBOL_MAX)
			} else if (out.max !== undefined && out.max !== null) {
				out.max = clampInt(out.max, DEFAULT_SYMBOL_MAX, HARD_SYMBOL_MAX)
			} else {
				out.max_results = DEFAULT_SYMBOL_MAX
			}
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
		instructions?: string
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
	instructions?: string
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
	/** Server-provided instructions from the MCP initialize handshake (may be null). */
	private serverInstructions: string | null = null
	private starting: Promise<void> | null = null
	private stopped = false
	private restartAttempts = 0
	private restartTimer: ReturnType<typeof setTimeout> | null = null
	/** 最近 N 行 stderr（固定容量缓冲），进程退出重启时用于排查。 */
	private stderrBuffer: string[] = []
	/** 未以换行结尾的半行，等下一 chunk 拼上（chunk 边界可能切断一行）。 */
	private stderrCarry = ""
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
		// DEBUG 模式直接透传；否则保留最近 N 行，进程退出重启时打印排查。
		if (this.proc.stderr) {
			this.proc.stderr.on("data", (chunk: Buffer | string) => {
				const text = typeof chunk === "string" ? chunk : chunk.toString("utf8")
				if (process.env.CODEGRAPH_GO_DEBUG) {
					console.error(`[codegraph-go] ${text.trimEnd()}`)
					return
				}
				const lines = (this.stderrCarry + text).split("\n")
				this.stderrCarry = lines.pop() || ""
				for (const line of lines) {
					const trimmed = line.trimEnd()
					if (!trimmed) continue
					this.stderrBuffer.push(trimmed)
					if (this.stderrBuffer.length > STDERR_KEEP_LINES) {
						this.stderrBuffer.shift()
					}
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

		const initResult = await this.sendRequest(
			"initialize",
			{
				protocolVersion: "2024-11-05",
				capabilities: {},
				clientInfo: { name: "pi-codegraph-go", version: "0.4.0" },
			},
			START_TIMEOUT_MS,
		)
		this.serverInstructions = initResult.instructions ?? null
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
		const bounded = withToolDefaults(args)
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

	/** Instructions announced by the server during initialize (absent → null). */
	getServerInstructions(): string | null {
		return this.serverInstructions
	}

	/** 打印最近 stderr（若有）并清空缓冲；每次异常退出只打一次（error+exit 双触发不重复）。 */
	private dumpStderrBuffer(): void {
		const tail = this.stderrCarry.trimEnd()
		if (tail) {
			this.stderrBuffer.push(tail)
			if (this.stderrBuffer.length > STDERR_KEEP_LINES) {
				this.stderrBuffer = this.stderrBuffer.slice(-STDERR_KEEP_LINES)
			}
		}
		this.stderrCarry = ""
		if (this.stderrBuffer.length === 0) return
		const lines = this.stderrBuffer.slice(-STDERR_KEEP_LINES)
		console.warn(`[codegraph-go] last stderr before restart (${lines.length} lines):`)
		for (const line of lines) {
			console.warn(`[codegraph-go]   ${line}`)
		}
		this.stderrBuffer = []
	}

	/** Exponential backoff restart after unexpected exit (max ~5 tries). */
	private scheduleRestart() {
		if (this.stopped) return
		// 每次异常退出都先打印最近 stderr，再决定是否/如何重启。
		this.dumpStderrBuffer()
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

			let prompt = event.systemPrompt
			const serverInstructions = client.getServerInstructions()
			if (serverInstructions) {
				prompt +=
					`

## CodeGraph server instructions

${serverInstructions}`
			}
			prompt +=
				`

## CodeGraph Tools

索引根: \`${client.workdir}\`（${client.workdirNote}）
${projectLine}
单次结果预算约 ${OUTPUT_CHAR_CAP} 字 / ${OUTPUT_LINE_CAP} 行（过大才压缩；正常体量不砍）。`

			return { systemPrompt: prompt }
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
	// MCP v0.8+ exposes a single tool "codegraph" (action=…). Pi uses the same name;
	// the adapter only bridges stdio + budgets — no per-action MCP fan-out.
	const run = async (params: Record<string, unknown>) => {
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
			const result = await c.callTool("codegraph", params)
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
		name: "codegraph",
		label: "CodeGraph",
		description:
			'CodeGraph: code search & call-graph analysis. Actions: search(pattern,[glob],[path]), files(glob,[path]), explore([query],[path]), callees(name,[file]), callers(name,[file]), impact(name,[file]), node(file|[name],[line]), status([path]), affected(files,[depth]), communities([path]), store_fact(targetFile,content), search_facts([query],[targetFile]). Common params: path, glob, max.',
		promptSnippet: "Code search & call-graph analysis via unified action router",
		promptGuidelines: [
			"Use codegraph with action='explore' FIRST for codebase/symbol overview. Empty query = project overview.",
			"Use action='search' for regex/literal code search (file:line results).",
			"Use action='files' to list files by glob.",
			"Use action='callees'/'callers'/'impact' for call-graph analysis.",
			"Use action='node' for full symbol detail (location, signature, deps) or file reading with line numbers.",
			"Use action='affected' after editing to find tests to run.",
			"Use action='communities' for global architecture/module structure questions.",
			"Use action='store_fact'/'search_facts' for cross-session knowledge persistence.",
			"Use action='status' when results are unexpectedly empty to check index health.",
			"When the user names a project under a home workdir, pass path= that project yourself.",
		],
		parameters: Type.Object({
			action: Type.String({
				description: "Action to perform",
				enum: [
					"search", "files", "explore", "callees", "callers", "impact",
					"node", "status", "affected", "communities", "store_fact", "search_facts",
				],
			}),
			pattern: Type.Optional(Type.String({ description: "search: regex or literal pattern (ripgrep syntax)" })),
			name: Type.Optional(Type.String({ description: "callees/callers/impact/node: symbol name" })),
			file: Type.Optional(Type.String({ description: "node/callees/callers/impact: file path or basename to pin" })),
			query: Type.Optional(Type.String({ description: "explore/search_facts: symbol or free-text / search term" })),
			path: Type.Optional(Type.String({ description: "Most actions: subdirectory or project name (home mode)" })),
			glob: Type.Optional(Type.String({ description: 'search/files/callees/callers/impact: file glob filter, e.g. "*.go"' })),
			max: Type.Optional(Type.Number({ description: "search/files/explore/callees/callers/impact/communities/search_facts: result cap" })),
			ignore_case: Type.Optional(Type.Boolean({ description: "search: case-insensitive search" })),
			line: Type.Optional(Type.Number({ description: "node: pin definition at/around this line" })),
			includeCode: Type.Optional(Type.Boolean({ description: "node: include source body (default false)" })),
			symbolsOnly: Type.Optional(Type.Boolean({ description: "node: symbol map + dependents only" })),
			offset: Type.Optional(Type.Number({ description: "node: 1-based start line" })),
			limit: Type.Optional(Type.Number({ description: "node: max lines (cap 2000)" })),
			skipCode: Type.Optional(Type.Boolean({ description: "explore: omit source code (default true)" })),
			files: Type.Optional(Type.Array(Type.String(), { description: "affected: list of changed source files" })),
			depth: Type.Optional(Type.Number({ description: "affected: max dependency traversal depth (default 5, max 10)" })),
			filter: Type.Optional(Type.String({ description: "affected: custom glob to identify test files" })),
			minSize: Type.Optional(Type.Number({ description: "communities: minimum community size (default 3)" })),
			targetFile: Type.Optional(Type.String({ description: "store_fact/search_facts: target file path" })),
			targetSymbol: Type.Optional(Type.String({ description: "store_fact/search_facts: target symbol" })),
			targetLine: Type.Optional(Type.Number({ description: "store_fact: target line number" })),
			content: Type.Optional(Type.String({ description: "store_fact: fact content" })),
			author: Type.Optional(Type.String({ description: "store_fact: author" })),
			supersedes: Type.Optional(Type.Number({ description: "store_fact: fact id to replace" })),
			status: Type.Optional(Type.String({ description: "search_facts: filter by status (default 'active')" })),
			projectPath: Type.Optional(Type.String({ description: "absolute path inside a project (nearest .codegraph/)" })),
		}),
		async execute(_id, params) {
			// Forward the full payload (including action) to MCP tool "codegraph".
			return run(params as Record<string, unknown>)
		},
	})
}
