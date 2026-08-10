// CodeGraph-Go Extension for Pi
// Local codegraph-go MCP bridge with safety rails for SSH / big homes.
//
// Design (no prompt hooks, no command enumeration):
// 1) /root 和 $HOME 可以当工作目录 —— cg-go home-mode 只索引「像项目」的一级目录
// 2) 每次工具结果有默认上限 + 硬封顶（模型可略调高，但不能炸会话）
// 3) 超长结果做温和压缩（保头尾），正常体量原样返回
// 4) 用户说需求即可，不必写 path/命令；由模型自己补技术参数。插件不猜口令、不钩消息。
//    （「人话」= 怎么说；「需求」= 说什么。两码事，别混。）
// 5) 客户端按需拉起：session_start 只做可用性判定与工具注册，首次工具调用才 spawn；
//    越界会话（config 授权根之外）不注册工具、execute 返回不可用原因。
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
// 非 DEBUG 时保留最近多少行 stderr，异常退出时写入日志文件（仍照常 drain 防管道阻塞）。
const STDERR_KEEP_LINES = 20
const DIAG_TAG = "codegraph-go"
const DIAG_LOG_FILE = "codegraph-go.log"
/** Cache log dir path after first successful ensure (avoids mkdir per line). */
let diagLogPath: string | null = null

/**
 * TUI-safe diagnostics. console.* corrupts Pi's input row — only DEBUG may print.
 * Otherwise append to ~/.pi/agent/logs/codegraph-go.log.
 */
function diagLog(msg: string): void {
	const line = `[${DIAG_TAG}] ${msg}`
	if (process.env.CODEGRAPH_GO_DEBUG) {
		console.error(line)
		return
	}
	try {
		if (!diagLogPath) {
			const dir = path.join(os.homedir() || "/tmp", ".pi", "agent", "logs")
			fs.mkdirSync(dir, { recursive: true })
			diagLogPath = path.join(dir, DIAG_LOG_FILE)
		}
		fs.appendFileSync(diagLogPath, `${new Date().toISOString()} ${line}\n`)
	} catch {
		diagLogPath = null
	}
}

/** Drop slog INFO/DEBUG; keep ERROR/WARN and unknown lines. */
function isInterestingStderrLine(line: string): boolean {
	if (/\blevel=INFO\b/i.test(line) || /"level"\s*:\s*"INFO"/i.test(line)) return false
	if (/\blevel=DEBUG\b/i.test(line) || /"level"\s*:\s*"DEBUG"/i.test(line)) return false
	return true
}

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

/**
 * 与二进制 config.expandPath 同语义（L2）：展开开头 ~（或裸 ~）为 $HOME，
 * 再展开 $VAR / ${VAR}（未设置的变量展开为空串）；$HOME 不可解析时 ~ 原样
 * 保留。config workdirs 里写 ~/proj 或 $PROJECT_ROOT 时，Pi 侧授权根与
 * 二进制侧展开结果一致，会话 workdir 才对得上。
 */
function expandPath(p: string): string {
	if (!p) return p
	if (p === "~" || p.startsWith("~/")) {
		const home = os.homedir()
		if (home) {
			p = p === "~" ? home : path.join(home, p.slice(2))
		}
	}
	return p.replace(/\$([A-Za-z_][A-Za-z0-9_]*|\{[^}]*\})/g, (m, name) => {
		const key = name[0] === "{" ? name.slice(1, -1) : name
		const v = process.env[key]
		return v !== undefined ? v : ""
	})
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
 * - CODEGRAPH_GO_WORKDIR 强制覆盖最优先
 * - config 文件 workdirs[0] 为「主 workdir」（统一决议，见下）
 * - 人在某个仓库里 → 用该仓库（更准）
 * - 人在 $HOME//root → 允许，cg-go home-mode 只扫项目目录
 * - 裸 /、/home、/Users → 拒绝（除非 ALLOW_BROAD）
 *
 * 统一 workdir 决议（方案 2）：config 文件 workdirs 非空时，所有会话无论
 * cwd 在哪都决议到同一个 workdir（config 声明的第一个授权根），不再按 cwd
 * 各自 spawn per-root daemon。原因：
 * - 双 daemon flock 冲突：cwd=/root/codegraph-go 的会话会 spawn 独立的
 *   per-root daemon，持有 /root/codegraph-go/.codegraph/codegraph.db 的
 *   flock；主 daemon（workdir=/root）跨项目查询 path=codegraph-go 时要
 *   打开子项目 db，撞锁报 "codegraph.db in use by another process"。
 * - 索引数据单源：同一 workdir 决议 = 同一 daemon = 同一份索引，避免同一
 *   目录被两个进程各建一份索引（.codegraph/ 互踩）。
 * config 找不到/解析失败/列表为空时，才回退 cwd→git root→项目标记决议。
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

	// config 主 workdir（方案 2）：config workdirs 非空时统一决议到第一个
	// 授权根，不再按 cwd 决议（消除 per-root daemon 与主 daemon 的双 daemon
	// flock 冲突，索引数据单源）。expandPath 语义与 allowedRoots 一致：展开
	// 为空（如 $UNSET_VAR）的项跳过，取第一个可用的。
	const cfg = configFilePath()
	if (cfg) {
		const cfgWorkdirs = parseConfigWorkdirs(cfg)
		for (const raw of cfgWorkdirs) {
			const e = expandPath(raw)
			if (!e) continue
			const w = normPath(e)
			if (!allowBroad && isFilesystemRoot(w)) {
				return {
					ok: false,
					reason: `拒绝 workdir=${w}（系统根）。请用家目录或项目路径。`,
				}
			}
			const note = isHomeDir(w)
				? "config workdirs[0]（家目录模式：只索引像项目的一级目录）"
				: "config workdirs[0]"
			return { ok: true, workdir: w, note }
		}
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

// ---- 授权范围判定（与二进制 config 语义一致） ---------------------------------
// 二进制侧（internal/config）：config 文件 workdirs 是权威 allowlist；无 config /
// 解析失败 / 空列表时回落 $HOME；$HOME 不可解析 → 空列表（fail closed）。
// config 文件优先级：$CODEGRAPH_CONFIG > ./codegraph-config.yaml >
// ~/.config/codegraph/config.yaml。TS 侧用轻量行解析，不引入 YAML 依赖。

/** config 文件查找（与二进制 ConfigPath 同一优先级）。找不到返回 null。 */
function configFilePath(): string | null {
	const env = process.env.CODEGRAPH_CONFIG
	if (env) {
		// 与二进制 ConfigPath 同语义（L1）：$CODEGRAPH_CONFIG 指向不存在的文件
		// 时跳过，继续查本地/全局候选——env 指向死路径时仍能读到真实 config
		// 的 workdirs，授权根不会被错误收窄成 $HOME。
		if (fs.existsSync(env)) return env
		diagLog(
			`$CODEGRAPH_CONFIG=${env} 不存在，跳过并继续默认查找（与二进制 ConfigPath 一致）`,
		)
	}
	const local = path.resolve("./codegraph-config.yaml")
	if (fs.existsSync(local)) return local
	const home = os.homedir()
	if (home) {
		const p = path.join(home, ".config", "codegraph", "config.yaml")
		if (fs.existsSync(p)) return p
	}
	return null
}

/**
 * 轻量解析 config 的 workdirs 段。支持两种形态（YAML 允许混排）：
 * - block：`workdirs:` 键之后的 `- <path>` 列表项（可带缩进与行尾注释）
 * - flow：键行内联列表 `workdirs: [/root, /opt]`（逗号分隔，项可带引号
 *   与行尾注释，`]` 后可带行尾注释）
 * 下一个顶格非列表项键结束该段（flow 键行后若还有 `- <path>` 行继续收）。
 *
 * 限制（M8）：不做完整 YAML，与 Go 端 yaml.v3 不同构。轻量解析器无法忠实
 * 处理的构造（锚点 &、别名 *、多文档 ---、块标量 |/>、flow 列表内嵌 {…}
 * mapping）一律整体判为「不可解析」→ 返回空列表 → 调用方按与 Go 端
 * WorkdirAllowlist 相同的语义回退到 $HOME（Go 对不可解析/空 workdirs 的
 * config 文件也是 $HOME 兜底），绝不拿猜出来的子集当授权根。
 * 真正的授权校验始终在二进制侧（main.go ValidateWorkdirs）执行，Pi 侧只是
 * 会话级 UX 判定；解析失败不会静默放行任何路径。
 */
function parseConfigWorkdirs(filePath: string): string[] {
	try {
		const data = fs.readFileSync(filePath, "utf8")
		if (
			/&[A-Za-z0-9_-]+/.test(data) || // 锚点定义（workdirs: &defaults）
			/\*[A-Za-z0-9_-]+/.test(data) || // 别名引用（- *defaults）
			/^---\s*$/m.test(data) || // 多文档分隔
			/^[ \t]*[|>][-+]?\s*$/m.test(data) // 块标量指示符
		) {
			diagLog(
				`config ${filePath} 含轻量解析器无法处理的 YAML 构造` +
					`（锚点/别名/多文档/块标量），授权根按「config 不可解析」回退到 $HOME` +
					`（与二进制 WorkdirAllowlist 语义一致）`,
			)
			return []
		}
		const roots: string[] = []
		let inWorkdirs = false
		for (const raw of data.split("\n")) {
			const line = raw.trimEnd()
			if (!inWorkdirs) {
				// flow 风格键行：`workdirs: [a, b]` 同行解析内联列表
				const flow = line.match(/^workdirs\s*:\s*\[(.*)\]\s*(?:#.*)?$/)
				if (flow) {
					if (/[{}]/.test(flow[1])) {
						// flow mapping 嵌在列表里（workdirs: [/a, {b: c}]）——无法解析，
						// 整体回退，不猜子集。
						diagLog(
							`config ${filePath} 的 workdirs 含 flow mapping` +
								`（{…}），轻量解析器不支持，回退到 $HOME`,
						)
						return []
					}
					inWorkdirs = true
					for (const item of flow[1].split(",")) {
						const p = item.trim().replace(/^["']|["']$/g, "").replace(/\s+#.*$/, "")
						if (p) roots.push(p)
					}
					continue
				}
				if (/^workdirs\s*:/.test(line)) inWorkdirs = true
				continue
			}
			// 顶格且不是列表项 → 下一个顶层键，workdirs 段结束
			if (/^\S/.test(line) && !/^-\s/.test(line)) {
				inWorkdirs = false
				continue
			}
			const m = line.match(/^\s*-\s+(.+)$/)
			if (m) {
				const p = m[1].trim().replace(/^["']|["']$/g, "").replace(/\s+#.*$/, "")
				if (p) roots.push(p)
			}
		}
		return roots
	} catch (e) {
		diagLog(`无法读取 config ${filePath}（${e}），授权根回退到 $HOME`)
		return []
	}
}

/**
 * 授权根列表：config 文件 workdirs（canonical、去重）；无 config / 解析失败 /
 * 空列表时回落 $HOME（canonical）。$HOME 不可解析 → 空列表（fail closed，
 * 与二进制 ValidateWorkdirs 语义一致）。
 */
export function allowedRoots(): string[] {
	const cfg = configFilePath()
	if (cfg) {
		const parsed = parseConfigWorkdirs(cfg)
		if (parsed.length > 0) {
			const roots: string[] = []
			const seen = new Set<string>()
			for (const r of parsed) {
				// 与二进制 WorkdirAllowlist 同语义（L2）：先 expandPath（~ 与
				// $VAR 展开），展开为空则跳过该 root，再做 canonical（normPath）。
				const e = expandPath(r)
				if (!e) continue
				const c = normPath(e)
				if (c && !seen.has(c)) {
					seen.add(c)
					roots.push(c)
				}
			}
			return roots
		}
		// 文件存在但解析失败（parseConfigWorkdirs 已打 warning）或 workdirs 为空：
		// 与二进制 WorkdirAllowlist 完全一致地回退到 $HOME（Go 对「不可解析 /
		// 空列表」的 config 文件同样是 $HOME 兜底），不静默放行、也不擅自收窄。
		// 二进制侧的 ValidateWorkdirs 才是真正的授权校验。
		diagLog(
			`config ${cfg} 未解析出 workdirs，授权根回退到 $HOME（与二进制语义一致）`,
		)
	}
	const home = normPath(os.homedir())
	return home ? [home] : []
}

/**
 * 路径段级包含判定（filepath.Rel 语义）：cand 等于某 root 或在其子树内为
 * true；兄弟前缀（/root-other vs /root）拒绝。两侧都 canonical。
 */
export function isWithinAllowlist(cand: string, roots: string[]): boolean {
	const c = normPath(cand)
	for (const r of roots) {
		const root = normPath(r)
		const rel = path.relative(root, c)
		if (rel === "") return true
		if (rel === ".." || rel.startsWith(".." + path.sep)) continue
		if (path.isAbsolute(rel)) continue // 不同盘/根（Windows 等）
		return true
	}
	return false
}

/**
 * 会话工作目录决策：resolveWorkdir 之后对最终 workdir 做授权范围校验。
 * 越界（config 授权根之外）→ { ok:false, reason }。resolveWorkdir 原有逻辑
 * （git 根/项目标记/家目录模式/系统根拒绝/ALLOW_BROAD）全部保留。
 */
export function makeWorkdirDecision(cwd: string): WorkdirDecision {
	const base = resolveWorkdir(cwd)
	if (!base.ok) return base
	const roots = allowedRoots()
	if (!isWithinAllowlist(base.workdir, roots)) {
		const declared = roots.length > 0 ? roots.join(", ") : "无"
		return {
			ok: false,
			reason: `workdir ${base.workdir} 不在 codegraph 授权根内（config 声明：${declared}）`,
		}
	}
	return base
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
	/** 最近 N 行 stderr（固定容量缓冲），异常退出时写入诊断日志。 */
	private stderrBuffer: string[] = []
	/** 未以换行结尾的半行，等下一 chunk 拼上（chunk 边界可能切断一行）。 */
	private stderrCarry = ""
	/** stop()/替换旧进程时置 true，避免 exit 监听把预期关闭当成故障。 */
	private intentionalClose = false
	readonly workdir: string
	readonly workdirNote: string

	constructor(workdir: string, workdirNote = "cwd") {
		this.workdir = workdir
		this.workdirNote = workdirNote
	}

	async start(): Promise<void> {
		if (this.proc && this.initialized) return
		if (this.starting) return this.starting
		this.starting = this.doStart().finally(() => {
			this.starting = null
		})
		return this.starting
	}

	/** 摘监听后 kill；配合 shutDownProc 先 cleanup，避免 pending 悬挂。 */
	private disposeProc(proc: ChildProcess): void {
		this.intentionalClose = true
		try {
			proc.removeAllListeners("exit")
			proc.removeAllListeners("error")
		} catch {
			/* ignore */
		}
		try {
			proc.stdin?.end()
		} catch {
			/* ignore */
		}
		try {
			proc.kill()
		} catch {
			/* ignore */
		}
	}

	/** dispose + 清 stderr + reject pending（doStart 换进程 / stop 共用）。 */
	private shutDownProc(): void {
		if (!this.proc) return
		this.disposeProc(this.proc)
		this.clearStderrBuffer()
		this.cleanup()
	}

	private pushStderrLine(line: string): void {
		if (!line) return
		this.stderrBuffer.push(line)
		if (this.stderrBuffer.length > STDERR_KEEP_LINES) this.stderrBuffer.shift()
	}

	private async doStart(): Promise<void> {
		if (this.proc) this.shutDownProc()

		const bin = process.env.CODEGRAPH_GO_BIN || "codegraph-go"
		this.intentionalClose = false
		this.proc = spawn(bin, ["-workdir", this.workdir], {
			stdio: ["pipe", "pipe", "pipe"],
		})

		// 按需模型：异常退出只记诊断日志 + cleanup；下次 callTool 再 start。
		// 禁止 console.*（污染 Pi TUI 输入行）。
		this.proc.on("error", (err) => {
			if (!this.intentionalClose) {
				diagLog(`process error: ${err.message}`)
				this.dumpStderrBuffer(true)
			} else {
				this.clearStderrBuffer()
			}
			this.cleanup()
		})

		this.proc.on("exit", (code, signal) => {
			const intentional = this.intentionalClose
			this.intentionalClose = false
			if (intentional) {
				this.clearStderrBuffer()
				this.cleanup()
				return
			}
			// 意外退出写文件一行（仍不 console）。0/null：proxy 断连或信号。
			const clean = code === 0 || code === null
			if (clean) {
				diagLog(
					`unexpected exit code=${code} signal=${signal ?? ""} (silent to TUI; next call will restart)`,
				)
				this.clearStderrBuffer()
			} else {
				diagLog(`process exited abnormally code=${code} signal=${signal ?? ""}`)
				this.dumpStderrBuffer(true)
			}
			this.cleanup()
		})

		// Drain stderr（防管道堵）；DEBUG 透传，否则入环缓冲。
		if (this.proc.stderr) {
			this.proc.stderr.on("data", (chunk: Buffer | string) => {
				const text = typeof chunk === "string" ? chunk : chunk.toString("utf8")
				if (process.env.CODEGRAPH_GO_DEBUG) {
					diagLog(text.trimEnd())
					return
				}
				const lines = (this.stderrCarry + text).split("\n")
				this.stderrCarry = lines.pop() || ""
				for (const line of lines) this.pushStderrLine(line.trimEnd())
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

		diagLog(
			`started workdir=${this.workdir} (${this.workdirNote}), ` +
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

	private flushStderrCarry(): void {
		const tail = this.stderrCarry.trimEnd()
		if (tail) this.pushStderrLine(tail)
		this.stderrCarry = ""
	}

	private clearStderrBuffer(): void {
		this.flushStderrCarry()
		this.stderrBuffer = []
	}

	/** 异常退出写最近 stderr（滤 INFO/DEBUG）；force 时过滤空则回退末 5 行。 */
	private dumpStderrBuffer(force = false): void {
		this.flushStderrCarry()
		if (this.stderrBuffer.length === 0) return
		const all = this.stderrBuffer.slice(-STDERR_KEEP_LINES)
		this.stderrBuffer = []
		let lines = all.filter(isInterestingStderrLine)
		if (lines.length === 0 && force) lines = all.slice(-5)
		if (lines.length === 0) return
		diagLog(`last stderr before exit (${lines.length} lines):`)
		for (const line of lines) diagLog(`  ${line}`)
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
		this.shutDownProc()
	}
}

/**
 * 直接以「已决议」workdir 启动客户端，不做任何二次决议（不 consult
 * CODEGRAPH_GO_WORKDIR / cwd）。决议只发生一次（session_start / 手动
 * codegraph-start），启动阶段不得再 consult env —— 否则 env 会把已过
 * allowlist 的决议 workdir 改道，且改道结果不再过授权校验（M4）。
 */
async function startClientAt(
	workdir: string,
	note: string,
): Promise<{ client: CodeGraphClient } | { error: string }> {
	try {
		const client = new CodeGraphClient(workdir, note)
		await client.start()
		return { client }
	} catch (err) {
		const msg = err instanceof Error ? err.message : String(err)
		diagLog(`failed to start: ${msg}`)
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
	/** 本会话工作目录决策（session_start 时刷新；execute 按它惰性判定）。 */
	let decision: WorkdirDecision | null = null
	let toolsRegistered = false
	/** 按需启动互斥：并发首次调用只 spawn 一次。 */
	let lazyStartPromise: Promise<CodeGraphClient> | null = null

	/**
	 * 惰性取客户端：优先返回已启动 client（手动 codegraph-start 拉起后工具
	 * 直接可用，不再误报「尚未初始化」）；decision 不可用返回不可用原因；
	 * 无客户端且 decision.ok 时首次调用才 spawn（工作目录用决议 workdir，
	 * 启动阶段不二次决议）。启动失败返回错误串，下一次调用重试。
	 */
	async function getClient(): Promise<CodeGraphClient | string> {
		if (client) return client
		const d = decision
		if (!d) return "codegraph-go 尚未初始化（尚无会话决策）"
		if (!d.ok) return d.reason
		if (!lazyStartPromise) {
			lazyStartPromise = (async () => {
				const result = await startClientAt(d.workdir, d.note)
				if ("client" in result) return result.client
				throw new Error(result.error)
			})()
		}
		try {
			const c = await lazyStartPromise
			client = c
			return c
		} catch (err) {
			const msg = err instanceof Error ? err.message : String(err)
			diagLog(`on-demand start failed: ${msg}`)
			return `codegraph-go 启动失败: ${msg}`
		} finally {
			lazyStartPromise = null
		}
	}

	pi.on("session_start", async (_event, ctx) => {
		// 只做决策 + 注册，不 spawn。客户端由首次工具调用按需拉起。
		decision = makeWorkdirDecision(ctx.cwd)
		if (decision.ok) {
			// 工作目录变了：停掉旧客户端，下次调用按新 decision 惰性重启
			if (client && client.workdir !== decision.workdir) {
				client.stop()
				client = null
			}
			if (!toolsRegistered) {
				registerTools(pi, getClient)
				toolsRegistered = true
			}
			const projects = isHomeDir(decision.workdir) ? listProjects(decision.workdir) : []
			const hint =
				projects.length > 0
					? ` | 项目: ${projects.slice(0, 10).join(", ")}${projects.length > 10 ? "…" : ""}`
					: ""
			ctx.ui.notify(
				`codegraph-go 可用（按需）：${decision.workdir}（${decision.note}）${hint}`,
				"info",
			)
		} else {
			// 越界/不可用会话：停掉可能残留的客户端（本会话用不上），不注册工具
			if (client) {
				client.stop()
				client = null
			}
			ctx.ui.notify(`codegraph-go 不可用: ${decision.reason}`, "warning")
		}
	})

	pi.on("before_agent_start", async (event, _ctx) => {
		// 只追加说明，不解析用户口令、不改焦点、不拦截消息。
		if (!decision) return
		if (!decision.ok) {
			return {
				systemPrompt:
					event.systemPrompt +
					`

## CodeGraph Tools

本会话 CodeGraph 未启用: ${decision.reason}
请用普通文件工具，或设置 CODEGRAPH_GO_WORKDIR。`,
			}
		}

		const home = isHomeDir(decision.workdir)
		const projects = home ? listProjects(decision.workdir) : []
		const projectLine = home
			? `家目录模式已开：索引里只有像项目的一级目录${projects.length ? `（${projects.join(", ")}）` : ""}。用户点名项目时代理自动补 path=，不依赖用户传。`
			: `当前索引根就是单个项目目录，一般不必再加 path。`

		let prompt = event.systemPrompt
		// client 已连接时若有 serverInstructions 仍可附加；按需未启动则省略该段。
		const serverInstructions = client ? client.getServerInstructions() : null
		if (serverInstructions) {
			prompt +=
				`

## CodeGraph server instructions

${serverInstructions}`
		}
		prompt +=
			`

## CodeGraph Tools

索引根: \`${decision.workdir}\`（${decision.note}）
${projectLine}
单次结果预算约 ${OUTPUT_CHAR_CAP} 字 / ${OUTPUT_LINE_CAP} 行（过大才压缩；正常体量不砍）。`

		return { systemPrompt: prompt }
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
			// 显式手动启动：立即 spawn（仍先过决策与授权范围校验）
			const d = makeWorkdirDecision(ctx.cwd)
			if (!d.ok) {
				ctx.ui.notify(`启动失败: ${d.reason}`, "error")
				return
			}
			const result = await startClientAt(d.workdir, d.note)
			if ("client" in result) {
				client = result.client
				// 把本次手动决策写回会话决策：后续工具调用直接复用已启动 client，
				// before_agent_start / codegraph-info 等按 decision 展示的段落
				// 也与实际运行状态一致（不再出现「已拉起仍报未初始化/未启用」）。
				decision = d
				if (!toolsRegistered) {
					registerTools(pi, getClient)
					toolsRegistered = true
				}
				ctx.ui.notify(
					`已启动: ${client.workdir}（${client.workdirNote}），${client.getTools().length} 个工具`,
					"info",
				)
			} else {
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
			// decision 保留：下次工具调用按同一决议惰性重启（按需拉起语义）。
			ctx.ui.notify("已停止", "info")
		},
	})

	pi.registerCommand("codegraph-info", {
		description: "查看 codegraph 工作目录与上限",
		handler: async (_args, ctx) => {
		// 会话已有决议时以其为准（手动 codegraph-start 写回后可能与重新推导
		// 一致），保证「解析」行与实际使用的 workdir 一致。
			const d = decision ?? makeWorkdirDecision(ctx.cwd)
			const projects =
				d.ok && isHomeDir(d.workdir) ? listProjects(d.workdir) : []
			const lines = [
				`cwd: ${ctx.cwd}`,
				`解析: ${d.ok ? `${d.workdir}（${d.note}）` : `拒绝 — ${d.reason}`}`,
				`授权根: ${allowedRoots().join(", ") || "（无）"}`,
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

function registerTools(pi: ExtensionAPI, getClient: () => Promise<CodeGraphClient | string>) {
	// MCP v0.8+ exposes a single tool "codegraph" (action=…). Pi uses the same name;
	// the adapter only bridges stdio + budgets — no per-action MCP fan-out.
	const run = async (params: Record<string, unknown>) => {
		const c = await getClient()
		if (typeof c === "string") {
			// decision 不可用 / 启动失败 / 未初始化：把原因直接给模型
			return {
				content: [
					{
						type: "text" as const,
						text: c,
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
			"Use action='search' for literal code search (file:line results); set regex=true for regex matching, no_ignore=true to include .gitignore'd files.",
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
			pattern: Type.Optional(Type.String({ description: "search: literal text to find (default); set regex=true for regular expression semantics" })),
			name: Type.Optional(Type.String({ description: "callees/callers/impact/node: symbol name" })),
			file: Type.Optional(Type.String({ description: "node/callees/callers/impact: file path or basename to pin" })),
			query: Type.Optional(Type.String({ description: "explore/search_facts: symbol or free-text / search term" })),
			path: Type.Optional(Type.String({ description: "Most actions: subdirectory or project name (home mode)" })),
			glob: Type.Optional(Type.String({ description: 'search/files/callees/callers/impact: file glob filter, e.g. "*.go"' })),
			max: Type.Optional(Type.Number({ description: "search/files/explore/callees/callers/impact/communities/search_facts: result cap" })),
			ignore_case: Type.Optional(Type.Boolean({ description: "search: case-insensitive search" })),
			regex: Type.Optional(Type.Boolean({ description: "search: treat pattern as a regular expression (default false: literal match)" })),
			no_ignore: Type.Optional(Type.Boolean({ description: "search: include .gitignore'd files (default false: ignore rules are respected)" })),
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
