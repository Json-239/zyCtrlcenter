// 机器人状态归类的**唯一口径**（Dashboard / MapView 共用；别再在页面里各写一份）。
//
// 权威定义在中控 Go 侧：
//   internal/services/waterline/waterline.go  Busy()   —— 水位压号 / "不打扰"
//   internal/services/roampool/roampool.go    Idle() = 在线 && !Busy() —— 游荡池派活
// Go 的 Busy 黑名单（2026-09-23 commit ef6a044 补齐 SUBMIT/ERROR）：
//   战斗(r.Fight) / 活跃抓鬼会话(ghost.enabled) / 游荡或孵化(walk.enabled、hatch.active) /
//   NAV·CLICK·DIALOG·FIGHT·SHOP·ALLOC·WAIT_NEXT·SUBMIT（交付/提交中，推进态）/
//   WAIT_TASK 且 task_index≠0 / ERROR
//
// 前端展示把 ERROR 从"忙"里单列成「异常」（卡住/停链，先人工处理，别派活）——
// 与 Go 把 ERROR 并入 Busy 的**结论等价**：既不进"空闲"候选，也不当"在干活"。
// 2026-09-23 抽出本模块：大屏 Dashboard 原先漏了两条（TASK_STATES 缺 SUBMIT、isStuck
// 要求 err_code 非空），导致 SUBMIT 号与 ERROR 号被同时计进「发呆」与「卡住」；
// 地图页 MapView 已对齐（26f4660），这里把判据抽成一份供两页共用，避免第三套口径。

// 推进中的任务态（与 Go waterline.Busy 的 switch 分支逐字对应；ERROR 单列见 isErr）
export const BUSY_STATES = ['NAV', 'CLICK', 'DIALOG', 'FIGHT', 'SHOP', 'ALLOC', 'WAIT_NEXT', 'SUBMIT']

// 机器人上报的卡住/停链态（如"换图推送迟迟未到"）→ 前端「异常」桶
export function isErr(r) { return String(r.state || '').toUpperCase() === 'ERROR' }

// 活跃抓鬼会话（ghost 字段非空只代表该号有 GhostState；停止/跑完的号 enabled=false 也会上报，不能只看字段存在）
export function isGhosting(r) { return !!(r.ghost && r.ghost.enabled === true) }

// 孵化会话进行中（孵化白名单图 6/17/34/40）：孵化也是"游荡到孵化图"，本身就在游荡
export function isHatching(r) { return !!(r.hatch && r.hatch.active === true && r.hatch.hatched !== true) }

// 游荡中（含孵化）：与地图页绿环、Go r.Walking() 同一判据
export function isWalking(r) { return !!(r.walk && r.walk.enabled === true) || isHatching(r) }

// 组队待命（2026-09-29 阶段 2）：机器人端 `state=MEMBER` = 队员在队待命
// （不接任务/不找鬼/不发移动，只参战）。在队期间**不该被派活**（派了也会被机器人端
// 忽略；Go 侧已把队内号从任务/游荡派发里剔除）——归属独立的「待命」桶，
// 既不算空闲（否则会被面板/批量操作误选去游荡），也不算发呆/未知。
export function isTeamStandby(r) { return String(r.state || '').toUpperCase() === 'MEMBER' }

// 忙碌 = 战斗 / 抓鬼 / 游荡(含孵化) / 推进中的任务态 / WAIT_TASK 且有点名中的任务。
// 注意：ERROR 不在这里（由 isErr 单列「异常」）——比 Go 的 waterline.Busy 少一个 ERROR 项，
// 只是因为前端展示把 ERROR 单列成「异常」桶；判断"忙不忙"的调用方要连同 isErr 一起看
// （或直接用 isIdle() / bucketOf()），两者结论一致。
// 地图页的保守取向（对下游派活宁可偏保守）：/api/random_walk 下游只校验"在线"、不看状态，
// 派错就会打断任务链，所以 SUBMIT 这类"推进中"必须算忙（Go 侧 2026-09-23 已同口径补齐）。
export function isBusy(r) {
  if (r.fight || isGhosting(r) || isWalking(r)) return true
  const s = String(r.state || '').toUpperCase()
  if (BUSY_STATES.includes(s)) return true
  if (s === 'WAIT_TASK') return Number(r.task_index) !== 0
  return false
}

// 空闲（在线 且 非忙 且 非异常 且 非组队待命）：与 Go roampool.Idle 结论一致（Go 把 ERROR 并入 Busy）。
// WAIT_GHOST（钟馗等刷鬼的"等待段"）不在黑名单里 → 无活跃抓鬼会话时算空闲（与 Go 侧注释一致；
// 有活跃抓鬼会话时 isGhosting 已把它判成忙，两处不打架）。
// MEMBER（组队待命，2026-09-29）单独排除：它不是"余量号"，不该出现在空闲候选/批量派活里。
export function isIdle(r) { return !!r.online && !isBusy(r) && !isErr(r) && !isTeamStandby(r) }

// 互斥归类（按"最该先看到"的优先级取一个）——地图页筛选按钮与排序共用；
// BUCKET_ORDER 是排序权重（空闲最前，异常最后；待命排在异常前）
export const BUCKET_ORDER = { idle: 0, full: 1, task: 2, ghost: 3, walk: 4, team: 5, err: 6 }
export function bucketOf(r) {
  // ERROR（机器人上报的卡住/停链）**最优先**：Go 的 waterline.Busy() 黑名单也含它，
  // 若按"非忙碌=空闲"会把它归进「空闲」——派活只会让卡住号更难处理。这里单列一类。
  if (isErr(r)) return 'err'
  if (isWalking(r)) return 'walk'
  if (isGhosting(r)) return 'ghost'
  if (isTeamStandby(r)) return 'team' // 组队待命：独立一类（不算空闲/不发呆）
  if (isBusy(r)) return 'task'
  if (r.ghost_done_today) return 'full'
  return 'idle'
}
