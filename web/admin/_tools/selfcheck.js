#!/usr/bin/env node
/* 闸门体检：把每个闸门**弄坏一次**，确认 check.js 真的以退出码 1 结束。
 *
 * 为什么需要它：闸门返回的失败数要汇总成退出码，一旦某一步返回 undefined（例如函数
 * 忘了 return），整个加总变成 NaN —— NaN 是 falsy，于是"打印了一堆 FAIL、最后说全部通过、
 * 退出码 0"。闸门就成了摆设，比没有还危险（让人以为验过了）。
 *
 * 做法：整个 web/admin 拷到临时目录（check.js 的 ADMIN_DIR 按自身位置推导），在副本里
 * 破坏一处，跑一次，断言退出码正好是 1 —— 工作区不动。
 *
 * 用法: node web/admin/_tools/selfcheck.js
 */
const fs = require('fs')
const os = require('os')
const path = require('path')
const { spawnSync } = require('child_process')

const ADMIN_DIR = path.resolve(__dirname, '..')
const TMP = path.join(os.tmpdir(), 'admin-selfcheck')

// [说明, 模式, 文件, 原文, 改成]
const CASES = [
    ['setup 返回未定义的名字', 'sfc', 'pages/App.vue', 'var loginRealms = ref([])', 'var loginRealmsRenamed = ref([])'],
    ['App.vue 挂载闸门', 'render', 'pages/App.vue', 'Panel.onError(', 'Panel.onErrorX('],
    ['登录渠道下拉闸门', 'render', 'pages/App.vue', ':label="r.name" :value="r.name"', ':label="r.realm" :value="r.realm"'],
    ['编辑器 facts 来自详情闸门', 'render', 'pages/NodeEditDialog.vue',
        'this.detail = full', 'this.detail = null'],
    // 注入**旧的 fail-open 写法**: 缺 editable 时当成"全都能写"
    // 非 auth 类型的抽屉不该去问凭据端点（问了就是 400 + 全局错误提示）
    // 判据依赖 this.def ⇒ 先开过一个能登录的节点再开 signup 就会去问端点
    // isAuthType 掉进 methods ⇒ 模板拿到函数对象, 守卫永远为真（真实踩过的那个 bug）
    ['refLabel 用服务端 display 闸门', 'render', 'js/api.js',
        'const serverLabel = (n.extra || {}).display', 'const serverLabel = undefined'],
    ['isAuthType 必须在 computed 闸门', 'render', 'pages/NodeEditDialog.vue',
        'isAuthType() {', 'isAuthTypeMovedOut() {'],
    ['isAuthType 依赖 def 闸门', 'render', 'pages/NodeEditDialog.vue',
        'var type = (this.node && this.node.type) || this.typeName',
        'var type = (this.def && this.def.name) || this.typeName'],
    // 列表请求不静默 ⇒ 非 owner/非 auth 类型会弹全局错误提示
    ['凭据列表静默闸门', 'render', 'js/api.js',
        '{ quiet: true }', '{ quiet: false }'],
    // 只在 mounted 里加载 ⇒ 换节点仍然显示第一个节点的凭据
    ['面板随节点刷新闸门', 'render', 'pages/AuthPanel.vue',
        'nodeId() { this.reload() },', 'nodeId() { return 0 },'],
    // 后台品牌硬编码框架名（站点名显示不出来）
    ['后台品牌该用站点名闸门', 'render', 'pages/App.vue',
        '{{ siteLabel }}', 'GCM'],
    // 反向引用块掉进 methods ⇒ v-if 恒真（整个抽屉每次都去查）
    ['inbounds 块必须在 computed 闸门', 'render', 'pages/NodeEditDialog.vue',
        'showsInbounds() {', 'showsInboundsFn() {'],
    // 反向引用面板换节点不重查 ⇒ 显示上一个节点的入边
    ['inbounds 面板随节点刷新闸门', 'render', 'pages/InboundPanel.vue',
        'nodeId() { this.load() },', 'nodeId() { return 0 },'],
    // 面板搬进 AuthPanel.vue 了: 注入"根元素丢守卫"（非 owner 会看到别人的凭据）
    ['凭据面板缺 isOwner 守卫闸门', 'render', 'pages/AuthPanel.vue',
        '<div v-if="isOwner" class="auth-block">',
        '<div class="auth-block">'],
    ['缺 editable 不许 fail-open 闸门', 'render', 'pages/NodeEditDialog.vue',
        'var editable = this.detail.editable || []',
        "var editable = this.detail.editable || (((this.def && this.def.fields) || []).map(function (f) { return f.name }))"],
    ['只读值没传 modelValue 闸门', 'render', 'pages/FieldRenderer.vue',
        'mode="view" :model-value="get(f.name)"', 'mode="view"'],
    ['widget 收到 view 掉进编辑器闸门', 'render', 'widgets/timestamp.vue',
        "v-if=\"mode !== 'edit'\"", "v-if=\"mode === 'cell'\""],
    ['只读详情闸门（view 被截断）', 'render', 'widgets/text.vue',
        '    <span v-else-if="mode === \'view\'" class="w-cell">{{ modelValue }}</span>',
        '    <span v-else-if="mode === \'view\'" class="w-cell">{{ Widgets.truncate(modelValue) }}</span>'],
    ['只读字段必须走 view 闸门', 'render', 'pages/FieldRenderer.vue', 'mode="view"', 'mode="cell"'],
    ['列表单元格 defs 闸门', 'render', 'pages/nodes.vue', ':field="fieldDef(c)" :defs="typeDefs"', ':field="fieldDef(c)"'],
    ['引用标签闸门（display 不优先）', 'render', 'js/api.js',
        "        const declared = ((def || {}).admin || {}).display", "        const declared = ''"],
    ['引用标签闸门（兜底成 #id）', 'render', 'js/api.js',
        "const cols = ((def || {}).admin || {}).columns || []", "return '#' + n.id"],
    ['列表时间列闸门（ISO 假设）', 'render', 'pages/nodes.vue', 'fmt(s) { return s ? Widgets.localTime(s) : \'\' }', "fmt(s) { return s ? s.replace('T', ' ').slice(0, 16) : '' }"],
    ['静态资源闸门', 'sfc', 'index.html', '<script src="js/widgets.js"></script>', '<script src="js/nope.js"></script>'],
    ['破坏性措辞闸门', 'sfc', 'pages/NodeOps.vue', '永久删除', '删除'],
    ['抽屉关闭闸门', 'sfc', 'pages/NodeEditDialog.vue', ':before-close="requestClose"', ''],
    ['时间控件闸门', 'sfc', 'widgets/timestamp.vue', 'type="datetime"', 'type="datetime" value-format="x"'],
    ['组件样式闸门（组件样式跑回公共表）', 'sfc', 'css/main.less', '.w-cell {', '.w-image { display:flex; }\n.w-cell {'],
    ['页面编译闸门', 'sfc', 'pages/nodes.vue', "name: 'NodesPage'", 'name: NodesPage'],
    ['组件 style 必须纯 CSS（// 注释）', 'sfc', 'widgets/number.vue', '<style>', '<style>\n// 数字：右对齐'],
    ['调度器不许认识 kind 名', 'render', 'pages/FieldRenderer.vue', "f.kind === 'array'", "f.kind === 'array' || f.kind === 'timestamp'"],
    ['cell 必须渲染出值', 'render', 'widgets/text.vue', 'Widgets.truncate(modelValue)', "''"],
    ['引用必须是链接', 'render', 'widgets/ref.vue', 'class="w-ref-link"', 'class="w-plain"'],
    ['点引用要能打开抽屉', 'render', 'pages/nodes.vue', 'typeName: target.type }', "typeName: '' }"],
    ['编辑器链路（穿异步组件）', 'render', 'widgets/text.vue', "this.$emit('update:modelValue', v)", 'void v'],
    ['组件接口闸门（kind 专用 prop 复活）', 'sfc', 'widgets/text.vue',
        '    props: {\n', '    props: {\n        preset: { type: Array, default: () => [] },\n'],
    ['嵌套 template 闸门（裸 <template> 包住 array）', 'sfc', 'pages/FieldRenderer.vue',
        "class=\"fr-array\">\n                        <div v-for=", "class=\"fr-array\">\n                    <template>\n                        <div v-for="],
    // 结构分支现在可能是 v-if 或 v-else-if（前面多了只读分支）—— 破坏点按"指令 + 条件"整段替换,
    // 不写死是哪个指令（写死了就会像这次一样: 代码一动, 破坏点找不到, 体检反而报错）。
    ['array 结构闸门（不渲染 array 分支）', 'render', 'pages/FieldRenderer.vue',
        "=\"f.kind === 'array'\"", '="false"'],
    ['时间旧值不回写闸门（去掉守卫）', 'render', 'widgets/timestamp.vue',
        'if (v === null && !this.parseable) return', ''],
    ['空值不算修改闸门（去掉 unchanged 守卫）', 'render', 'pages/FieldRenderer.vue',
        'if (unchanged(this.get(name), v)) return', ''],
    ['编辑器引用标签闸门（用了列表行的 expand）', 'sfc', 'pages/NodeEditDialog.vue',
        'this.refExpand = full.expand', 'this.refExpand = r.expand'],
    ['组件接口闸门（复活 /admin/expand）', 'sfc', 'pages/NodeOps.vue',
        'this.expandDialog.loading = true', "this.expandDialog.loading = true; window.$api.get('/admin/expand')"],
]

let bad = 0
for (const [name, mode, rel, from, to] of CASES) {
    fs.rmSync(TMP, { recursive: true, force: true })
    fs.cpSync(ADMIN_DIR, TMP, { recursive: true })
    const file = path.join(TMP, rel)
    const src = fs.readFileSync(file, 'utf8')
    if (src.indexOf(from) < 0) {
        console.log('  ?? ' + name + ': 找不到要破坏的文本（' + rel + ': ' + from.slice(0, 40) + '）')
        bad++
        continue
    }
    fs.writeFileSync(file, src.replace(from, to))
    const args = [path.join(TMP, '_tools/check.js')].concat(mode === 'sfc' ? [] : [mode])
    const res = spawnSync('node', args, { encoding: 'utf8' })
    if (res.status === 1) {
        console.log('  ok   ' + name + '（' + mode + '）→ 退出码 1')
    } else {
        bad++
        console.log('  FAIL ' + name + '（' + mode + '）→ 退出码 ' + res.status + '（闸门没拦住）')
        const lines = (res.stdout || '').trim().split('\n')
        console.log('       ' + lines[lines.length - 1])
    }
}
fs.rmSync(TMP, { recursive: true, force: true })
console.log(bad ? '\n' + bad + ' 个闸门没起作用' : '\n' + CASES.length + ' 个闸门都能让命令失败')
process.exit(bad ? 1 : 0)
