#!/usr/bin/env node
/*
 * 后台 UI 校验 — gcm 的 admin 是无构建 SFC（vue3-sfc-loader 在浏览器里现编译），
 * 改了 .vue 没有编译器兜底：模板写错只有打开页面才炸。这个脚本用仓库自带的
 * vendor/vue.global.prod.js + vendor/vue3-sfc-loader.js（与浏览器同一套运行时）
 * 在 node 里跑校验，不需要 npm install。
 *
 *   node web/admin/_tools/check.js            # 编译全部 pages/*.vue
 *   node web/admin/_tools/check.js render     # 挂载渲染 + 过滤行为回归
 *
 * 退出码非 0 = 有失败项。web/admin/_tools 以 _ 开头，不会被 //go:embed 打进二进制。
 */
'use strict'

const fs = require('fs')
const path = require('path')
const vm = require('vm')

// KERNEL_KINDS 内核 types 包声明的 kind（改动 types/*.go 时这里要跟着对）——
// 只用来在检查时提示"哪些 kind 在后台还没有控件"。
const KERNEL_KINDS = ['address', 'array', 'bool', 'gallery', 'multiselect', 'number', 'object',
    'ref', 'refs', 'richtext', 'select', 'text', 'textarea', 'timestamp',
    'upload-file', 'upload-image']

const ADMIN_DIR = path.join(__dirname, '..')
const PAGES_DIR = path.join(ADMIN_DIR, 'pages')

function read(file) { return fs.readFileSync(file, 'utf8') }

// ── 与浏览器等价的运行时（Vue + SFC 加载器 + 最小 DOM 桩） ──────────────
function loadRuntime() {
    const elem = () => {
        const el = {
            style: {}, setAttribute() {}, appendChild() {}, removeChild() {},
            classList: { add() {} }, textContent: '', children: [],
        }
        // 像浏览器那样：设 innerHTML 会把去掉标签的内容放进 textContent（Widgets.plain 依赖它）
        let html = ''
        Object.defineProperty(el, 'innerHTML', {
            get: () => html,
            set: (v) => { html = String(v); el.textContent = html.replace(/<[^>]*>/g, '') },
        })
        return el
    }
    const document = {
        head: elem(), body: elem(), createElement: elem, createElementNS: elem,
        createTextNode: () => ({ textContent: '' }), querySelector: () => null,
        querySelectorAll: () => [], documentElement: elem(),
        addEventListener() {}, getElementById: () => null,
    }
    const sandbox = {
        document, navigator: { userAgent: 'node' }, location: { href: 'http://localhost/admin/ui/' },
        console, setTimeout, clearTimeout, Promise, URL,
        fetch: () => Promise.reject(new Error('check.js 不联网')),
    }
    sandbox.window = sandbox
    sandbox.self = sandbox
    sandbox.globalThis = sandbox
    vm.createContext(sandbox)
    vm.runInContext(read(path.join(ADMIN_DIR, 'vendor/vue.global.prod.js')), sandbox, { filename: 'vue.js' })
    vm.runInContext(read(path.join(ADMIN_DIR, 'vendor/vue3-sfc-loader.js')), sandbox, { filename: 'loader.js' })
    const Vue = sandbox.Vue
    const loadModule = sandbox['vue3-sfc-loader'].loadModule

    // 模块 id 用 /pages/x.vue 形态（与浏览器一致：相对 import 靠它解析）
    //
    // `$api` 必须是**指向 sandbox.$api 的代理**, 不能是固定对象: 组件里写的是
    // `import $api from '$api'` ⇒ 加载那一刻拿到的引用会被缓存。写死一个 `{}` 的话,
    // 各处测试设的 sandbox.$api 全都进不去（踩过: 登录渠道那段闸门就这么静默失效,
    // 组件拿到空对象 → loginRealms is not a function）。浏览器里 panel.js 映射的是
    // window.$api, 同一个道理。
    const apiProxy = new Proxy({}, {
        get: (_, key) => sandbox.$api && sandbox.$api[key],
        has: (_, key) => Boolean(sandbox.$api && key in sandbox.$api),
    })
    const options = () => ({
        moduleCache: {
            vue: Vue,
            'vue-router': { useRouter: () => ({ afterEach: () => {}, push: () => {}, replace: () => {}, currentRoute: { value: {} } }), useRoute: () => ({ name: '', params: {}, query: {}, path: '/' }), createRouter: () => ({}), createWebHashHistory: () => ({}) },
            '$api': apiProxy,
        },
        async getFile(url) {
            const file = path.join(ADMIN_DIR, url.replace(/^\//, ''))
            return { getContentData: () => Promise.resolve(read(file)) }
        },
        addStyle() {},
        log(type, scope, message) { if (type === 'error') console.error('   [' + scope + ']', message) },
    })
    return { Vue, sandbox, loadComponent: (rel) => loadModule(rel, options()) }
}

function pageList() {
    return fs.readdirSync(PAGES_DIR).filter(f => f.endsWith('.vue')).sort()
}

// ── ⓿ 静态资源引用：index.html 里写到的文件必须真的存在 ──────────────
// 路径写错不会报错，只是 404 + 静默失效（favicon 没了、样式不生效），所以在这里钉住。
function checkAssets() {
    const html = read(path.join(ADMIN_DIR, 'index.html'))
    const refs = []
    for (const m of html.matchAll(/(?:href|src)="([^"]+)"/g)) {
        const url = m[1]
        if (/^(https?:)?\/\//.test(url) || url.startsWith('data:') || url.startsWith('#')) continue
        refs.push(url.startsWith('/admin/ui/') ? url.slice('/admin/ui/'.length) : url)
    }
    let failed = 0
    for (const rel of refs) {
        if (!fs.existsSync(path.join(ADMIN_DIR, rel))) {
            failed++
            console.log('  FAIL index.html 引用了不存在的文件: ' + rel)
        }
    }
    if (!failed) console.log('  ok   index.html 引用的静态资源都存在（' + refs.length + ' 个）')
    return failed
}

// ── ⓪b 破坏性操作：措辞与行为必须对得上 ──────────────────────────────
// NodeOps 的"删除"调的是 deleteNode（永久删除；被引用则拒绝），而归档节点连后台列表
// 都查不到（所有读路径都写死 archived_at IS NULL）—— 措辞含糊 = 运营当软删点下去。
// 编辑抽屉必须能"点外面就关"，且关之前要拦一下未保存的修改。
// 曾经写死 :close-on-click-modal="false" / :close-on-press-escape="false" —— 只能点关闭按钮，
// 手一滑就只能取消，改起来烦。element-plus 的默认值是 true，所以这两行不该出现。
function checkDrawerClose() {
    const src = read(path.join(ADMIN_DIR, 'pages/NodeEditDialog.vue'))
    const problems = []
    if (/:close-on-click-modal\s*=\s*"false"/.test(src)) problems.push('抽屉把"点遮罩关闭"关掉了')
    if (/:close-on-press-escape\s*=\s*"false"/.test(src)) problems.push('抽屉把 ESC 关闭关掉了')
    if (!/:before-close\s*=\s*"requestClose"/.test(src)) problems.push('抽屉缺 :before-close="requestClose"（未保存提示）')
    if (!/isDirty\s*\(/.test(src)) problems.push('没有脏检查 isDirty')
    if (!/ElMessageBox\.confirm/.test(src)) problems.push('没有确认弹窗 ElMessageBox.confirm')
    if (!/@click="requestClose\(\)"/.test(src)) problems.push('底部"取消"没走 requestClose（会绕过未保存提示）')
    let failed = 0
    for (const problem of problems) {
        failed++
        console.log('  FAIL ' + problem)
    }
    if (!failed) console.log('  ok   NodeEditDialog 点遮罩/ESC 可关，且关前拦未保存修改')
    return failed
}

function checkDestructiveWording() {
    const src = read(path.join(ADMIN_DIR, 'pages/NodeOps.vue'))
    const problems = []
    // 盯住按钮本身（只查"文件里出现过永久删除"太松：确认框里有，按钮上却可能写着"删除"）
    const btn = /@click="doDelete"[^>]*>([^<]*)</.exec(src)
    if (!btn || !btn[1].includes('永久删除')) problems.push('删除按钮文案不是"永久删除"')
    if (!src.includes('$api.deleteNode(')) problems.push('删除动作没调 deleteNode')
    if (src.includes('archiveNode(')) problems.push('NodeOps 里出现了 archiveNode')
    for (const msg of problems) console.log('  FAIL ' + msg)
    if (!problems.length) console.log('  ok   NodeOps 的删除明确标注为永久删除')
    return problems.length
}

// 时间字段存的是统一格式字符串（UTC + 秒精度 + Z），不是毫秒数。
// 曾经用 value-format="x" 提交毫秒：库里于是数字/字符串混排，SQL 侧排序和日期筛选全废。
// 组件样式：每个 kind 自己的样式写在它自己的组件里（loader 会把 <style> 注入 head）。
// 公共表（css/main.less）只许放"真公共"的那几个类 —— 组件自己的规则跑回公共表，
// 就是上一轮"控件搬走了、样式留在原地"那个 bug 的翻版。缩略图固定 40×40 裁切也钉在这。
const PUBLIC_WIDGET_CLASSES = ['.w-cell', '.w-empty', '.w-missing', '.w-ref-link']

function widgetStyleSources() {
    const out = {}
    for (const f of fs.readdirSync(path.join(ADMIN_DIR, 'widgets'))) {
        if (!f.endsWith('.vue')) continue
        const src = read(path.join(ADMIN_DIR, 'widgets', f))
        const m = src.match(/<style[^>]*>([\s\S]*?)<\/style>/)
        out[f] = m ? m[1] : ''
    }
    return out
}

function checkWidgetStyles() {
    const less = read(path.join(ADMIN_DIR, 'css/main.less'))
    const styles = widgetStyleSources()
    const all = less + Object.values(styles).join('\n')
    const problems = []
    const ruleIn = (src, cls) => (src.match(new RegExp('\\' + cls + '\\s*\\{([^}]*)\\}')) || [, ''])[1]

    // ① 缩略图：正方形裁切，别被大图撑开（尺寸本身由使用方定，这里只钉"是正方形且合理"）
    const thumb = ruleIn(all, '.w-thumb')
    if (!thumb) problems.push('没有 .w-thumb（列表缩略图会显示成原图大小）')
    else {
        const w = (thumb.match(/width:\s*(\d+)px/) || [])[1]
        const h = (thumb.match(/height:\s*(\d+)px/) || [])[1]
        if (!w || !h) problems.push('.w-thumb 缺固定宽高: ' + thumb.trim().replace(/\s+/g, ' '))
        else if (w !== h) problems.push('.w-thumb 宽高不等 (' + w + '×' + h + ')，缩略图会变形')
        else if (+w < 24 || +w > 64) problems.push('.w-thumb 尺寸不合理 (' + w + 'px)')
        if (!/object-fit:\s*cover/.test(thumb)) problems.push('.w-thumb 缺 object-fit: cover（会变形）')
    }

    // ④ 组件的 <style> 是**纯 CSS**（没有 less 预处理器）—— // 注释、& 嵌套、
    //    @变量 都会让紧随其后的规则被整段丢掉（页面看着像"样式没生效"）。
    for (const [f, css] of Object.entries(styles)) {
        if (!css) continue
        const body = css.replace(/\/\*[\s\S]*?\*\//g, '')
        if (/^\s*\/\//m.test(body)) problems.push(f + ' 的 <style> 里有 // 注释（CSS 不支持，会吞掉后面的规则）')
        if (/(^|\s)&\s*[.:]/.test(body)) problems.push(f + ' 的 <style> 里用了 & 嵌套（没有 less 预处理）')
        if (/@[a-z-]+\s*:/.test(body)) problems.push(f + ' 的 <style> 里用了 @ 变量（没有 less 预处理）')
        for (const decl of body.matchAll(/([a-z-]+)\s*:\s*[^;{}]+$/gm)) {
            problems.push(f + ' 的 <style> 里有没写完的声明（缺分号？）: ' + decl[0].trim())
        }
    }

    // ② 组件模板用到的 w-* 类必须有人定义（组件自己或公共表）
    const used = new Set()
    for (const [f, src] of Object.entries(styles)) {
        const body = read(path.join(ADMIN_DIR, 'widgets', f))
        for (const m of body.matchAll(/class="([^"]*)"/g)) {
            for (const cls of m[1].split(/\s+/)) if (cls.indexOf('w-') === 0) used.add(cls)
        }
    }
    for (const cls of used) {
        if (!new RegExp('\\' + cls + '\\s*\\{').test(all)) {
            problems.push('组件用到 .' + cls + ' 但没有任何样式定义（裸样式）')
        }
    }

    // ③ 公共表只许放公共类
    for (const m of less.matchAll(/^\.(w-[a-z-]+)\s*\{/gm)) {
        if (PUBLIC_WIDGET_CLASSES.indexOf('.' + m[1]) < 0) {
            problems.push('.' + m[1] + ' 是组件自己的样式，应写在它的组件里（css/main.less 只放 ' +
                PUBLIC_WIDGET_CLASSES.join(' ') + '）')
        }
    }

    for (const msg of problems) console.log('  FAIL ' + msg)
    if (!problems.length) {
        console.log('  ok   组件样式在各组件内（纯 CSS、缩略图正方形裁切；公共表只有 ' +
            PUBLIC_WIDGET_CLASSES.join(' ') + '）')
    }
    return problems.length
}

function checkTimestampWidget() {
    const src = read(path.join(ADMIN_DIR, 'widgets/timestamp.vue'))
    const problems = []
    if (/value-format\s*=\s*"x"/.test(src)) problems.push('日期选择器又用毫秒时间戳了（value-format="x"）')
    if (!/toUnix\s*\(/.test(src)) problems.push('缺 toUnix（提交前换算成 Unix 秒）')
    if (!/toDate\s*\(/.test(src)) problems.push('缺 toDate（显示时按本地时间还原）')
    if (!/Widgets\.localTime/.test(src)) problems.push('列表单元没有按本地时间显示（Widgets.localTime）')
    if (!/\*\s*1000/.test(src)) problems.push('缺 ×1000（JS 的 Date 是毫秒, 时间字段是秒）')
    for (const msg of problems) console.log('  FAIL ' + msg)
    if (!problems.length) console.log('  ok   timestamp 控件提交 Unix 秒（显示按本地时间）')
    return problems.length
}

// ── ① 编译校验：每个页面都能被 SFC 加载器编译 ────────────────────────
// 组件接口只有 node / field / mode：引用目标一律从 node.expand 取。
// 组件一旦重新长出 kind 专用 prop，父组件就又要知道"谁需要什么"，契约就退回去了。
// 引擎侧同理：只有一条读投影，不许再出现独立的 /admin/expand 出口。
function checkWidgetInterface() {
    const problems = []
    for (const file of fs.readdirSync(path.join(ADMIN_DIR, 'widgets'))) {
        if (!file.endsWith('.vue')) continue
        const src = read(path.join(ADMIN_DIR, 'widgets', file))
        for (const prop of ['preset', 'expand']) {
            if (new RegExp('^\\s*' + prop + ': \\{', 'm').test(src)) {
                problems.push('widgets/' + file + ' 又长出 kind 专用 prop「' + prop + '」（应该从 node 取）')
            }
        }
    }
    const walkFiles = (dir) => fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
        const full = path.join(dir, e.name)
        return e.isDirectory() ? walkFiles(full) : [full]
    })
    for (const file of walkFiles(ADMIN_DIR)) {
        if (!/\.(js|vue)$/.test(file)) continue
        if (file.indexOf(path.sep + '_tools' + path.sep) >= 0) continue // 闸门自己的源码里有这个字符串
        if (read(file).indexOf("'/admin/expand'") >= 0) {
            problems.push(path.relative(ADMIN_DIR, file) + ' 又出现 /admin/expand（读投影只有一条）')
        }
    }
    let failed = 0
    for (const problem of problems) { failed++; console.log('  FAIL ' + problem) }
    if (!failed) console.log('  ok   组件接口只有 node/field/mode；没有复活 /admin/expand')
    return failed
}

// 编辑器的引用标签必须来自"刚拉回来的详情"（full.expand）—— 用列表行的 r.expand 时，
// 打开编辑器常常没有 expand，引用字段就只剩裸 id，点开下拉才补上（真实踩过）。
// FieldRenderer 的 array 分支里曾经包了一层**没有指令**的 <template>：Vue 3 会把它编成
// 真 <template> 元素（不是 fragment）, 内容惰性 → 卡片建出来了却永远不可见。
// 规则：除了第 0 列的 SFC 根 template, 文件里不许有嵌套的 <template>。
function checkNoNestedTemplate() {
    const src = read(path.join(ADMIN_DIR, 'pages/FieldRenderer.vue'))
    const problems = []
    src.split('\n').forEach((line, i) => {
        // 只有"没有指令"的嵌套 <template> 才是坑（带 v-if/v-for/v-slot/# 的会编成 fragment, 正常）
        if (/^[ \t]+<template(?![^>]*(v-|#|:))[ >]/.test(line)) {
            problems.push('FieldRenderer.vue:' + (i + 1) + ' 有嵌套的 <template>（无指令时内容不显示）')
        }
    })
    let failed = 0
    for (const problem of problems) { failed++; console.log('  FAIL ' + problem) }
    if (!failed) console.log('  ok   FieldRenderer 没有嵌套 <template>（结构直接放在 v-if 元素里）')
    return failed
}

function checkEditorRefLabels() {
    const src = read(path.join(ADMIN_DIR, 'pages/NodeEditDialog.vue'))
    const problems = []
    if (!/this\.refExpand = full\.expand/.test(src)) {
        problems.push('loadEdit 没有用详情的 full.expand 填引用标签（引用字段会只剩裸 id）')
    }
    if (/this\.refExpand = r\.expand/.test(src)) {
        problems.push('loadEdit 用了列表行的 r.expand（打开编辑器时它常常是空的）')
    }
    if (!/:node="\{ fields: form\.fields, expand: refExpand \}"/.test(src)) {
        problems.push('没把 node 上下文传给字段渲染器（组件拿不到 node.expand）')
    }
    let failed = 0
    for (const problem of problems) { failed++; console.log('  FAIL ' + problem) }
    if (!failed) console.log('  ok   编辑器引用标签来自详情 expand（不是列表行）')
    return failed
}

// App.vue 的模板引用的名字必须都在 setup 的 return 里 —— 少一个, 页面打开就
// TypeError, 连登录表单都看不到（真实踩过: loginRealms）。只对 App.vue 做静态判定:
// 它是 setup + 显式 return 的根组件, 判定精确; 其它页面是 Options API（data/computed/
// methods）且模板结构复杂, 静态判定会误报 —— 那种情况静态看不懂就该让运行时看。
function checkAppReturns() {
    const src = read(path.join(PAGES_DIR, 'App.vue'))
    const tpl = src.slice(src.indexOf('<template>'), src.indexOf('</template>'))
    // 不去配对 setup 的整块（体内的字符串/注释里可能有花括号, 会把配对带偏 —— 踩过）:
    // 直接看 setup 之后的**最后一个** return {, 再从那一个大括号开始配对取键名。
    const body = src.slice(src.indexOf('setup('))
    const retAt = body.lastIndexOf('return {')
    let rd = 0, retEnd = body.length
    for (let k = body.indexOf('{', retAt); k < body.length; k++) {
        if (body[k] === '{') rd++
        else if (body[k] === '}') { rd--; if (rd === 0) { retEnd = k; break } }
    }
    const returned = new Set()
    for (const m of body.slice(retAt, retEnd).matchAll(/([A-Za-z_$][\w$]*)\s*:/g)) returned.add(m[1])
    const used = new Set()
    // 只对**取出来的表达式**剥字符串字面量（整模板剥会被文本里落单的引号吃掉一大段 —— 踩过）:
    // v-if="phase === 'login'" 里的 login 是字符串, 不是名字。
    const push = (expr) => {
        const noStr = expr.replace(/'(?:[^'\\]|\\.)*'/g, "''").replace(/"(?:[^"\\]|\\.)*"/g, '""')
        for (const m of noStr.matchAll(/(?<![.\w$])([A-Za-z_$][\w$]*)/g)) used.add(m[1])
    }
    for (const m of tpl.matchAll(/\{\{([^}]*)\}\}/g)) push(m[1])
    for (const m of tpl.matchAll(/\s(?:v-if|v-else-if|v-show|v-for|v-model(?::[\w-]+)?|:)(?:="|\s*=\s*")([^"]*)"/g)) push(m[1])
    for (const m of tpl.matchAll(/@[\w.-]+="([^"]*)"/g)) push(m[1])
    const local = new Set(['true', 'false', 'null', 'undefined', 'in', 'of', 'if', 'else', 'new',
        'typeof', 'return', 'this', 'length', 'item', 'key', 'index', 'value', '$event'])
    // 模板里自己声明的局部名（v-for 迭代变量 / 插槽作用域属性）—— 不需要 setup 返回
    for (const m of tpl.matchAll(/v-for="([^"]*)"/g)) {
        const head = m[1].split(/\s+(?:in|of)\s/)[0].replace(/[()]/g, '')
        for (const v of head.split(',')) {
            const name = v.split(':').pop().split('=')[0].trim()
            if (name) local.add(name)
        }
    }
    for (const m of tpl.matchAll(/(?:#|v-slot:)[\w-]*\s*=\s*"([^"]*)"/g)) {
        for (const v of m[1].replace(/[{}]/g, ' ').split(/[\s,]+/)) {
            const name = v.split(':').pop().split('=')[0].trim()
            if (name) local.add(name)
        }
    }
    const missing = [...used].filter(n => !returned.has(n) && !local.has(n)
        && !/^[A-Z][A-Za-z0-9]*$/.test(n) && n.indexOf('$') !== 0)
    if (missing.length) {
        console.log('  FAIL pages/App.vue'.padEnd(40) + '模板用到但 setup 没返回: ' + missing.join(', '))
        return 1
    }
    // 反方向也要查: **setup 返回的名字必须真的有定义**。少一个就是 mount 时
    // "ReferenceError: X is not defined" —— 页面整个白屏, 而上面那条（模板 → return）
    // 照样是绿的。真实踩过: 删旧功能时把 var globalQ 一起删了, return 里还留着。
    const undeclared = [...returned].filter(name =>
        !new RegExp('(?:var|let|const|function)\\s+' + name + '\\b').test(src))
    if (undeclared.length) {
        console.log('  FAIL pages/App.vue'.padEnd(40) +
            'setup 返回了没定义的名字（mount 时会 ReferenceError）: ' + undeclared.join(', '))
        return 1
    }
    console.log('  ok   ' + 'App.vue 的 setup 返回与模板引用一一对得上')
    return 0
}

async function checkSFC() {
    const { loadComponent } = loadRuntime()
    let failed = 0
    const targets = pageList().map(f => '/pages/' + f)
        .concat(fs.readdirSync(path.join(ADMIN_DIR, 'widgets'))
            .filter(f => f.endsWith('.vue')).map(f => '/widgets/' + f))
    for (const rel of targets) {
        try {
            const mod = await loadComponent(rel)
            const comp = mod.default || mod
            if (!(comp.render || comp.template || comp.setup)) throw new Error('没有 render/template/setup')
            console.log('  ok   ' + rel.padEnd(34) + (comp.name || ''))
        } catch (err) {
            failed++
            console.log('  FAIL ' + rel.padEnd(34) + String(err.message).split('\n')[0])
        }
    }
    return failed
}

// ── ② 挂载渲染 + 行为回归 ───────────────────────────────────────────
// 无浏览器挂载：自建渲染器 + el-* 桩件，只关心结构/类名与调用序列。
function makeRenderer(Vue) {
    const node = (tag) => ({ tag, children: [], props: {}, parent: null })
    const ops = {
        createElement: node,
        createText: (text) => ({ tag: '#text', text, children: [], props: {}, parent: null }),
        createComment: (text) => ({ tag: '#comment', text, children: [], props: {}, parent: null }),
        setText: (n, text) => { n.text = text },
        setElementText: (n, text) => { n.text = text },
        insert(child, parent, anchor) {
            child.parent = parent
            const i = anchor ? parent.children.indexOf(anchor) : -1
            if (i < 0) parent.children.push(child)
            else parent.children.splice(i, 0, child)
        },
        remove(child) {
            const p = child.parent
            if (p) p.children.splice(p.children.indexOf(child), 1)
        },
        parentNode: (n) => n.parent,
        nextSibling(n) {
            const p = n.parent
            return p ? p.children[p.children.indexOf(n) + 1] : null
        },
        patchProp(el, key, prev, next) { el.props[key] = next },
        querySelector: () => null,
        setScopeId() {},
        cloneNode: (n) => n,
    }
    return {
        renderer: Vue.createRenderer(ops),
        node,
        // el-* / 扩展控件桩件：保留 tag 便于断言结构
        stub: (name) => ({
            name,
            props: ['modelValue', 'data'],
            // attrs 里带着 @update:model-value 之类的监听器 —— 放到树上，闸门可以直接触发
            setup(props, { slots, attrs }) {
                // 表格列要提供作用域槽（{row}）: 真实 Element Plus 的 el-table 会把每一行传进来。
                // 桩件拿不到父表格的数据 ⇒ 用空的 row 兜 —— 目的是让模板里的取值路径跑一遍
                // （写错名字/取错层级会在这里报出来）, 不是校验内容。
                if (name === 'el-table-column' || name === 'el-table') {
                    const scope = { row: {}, column: {}, $index: 0 }
                    return () => Vue.h(name, { value: props.modelValue, ...attrs },
                        slots.default ? slots.default(scope) : [])
                }
                return () => Vue.h(name, { value: props.modelValue, ...attrs },
                    slots.default ? slots.default() : [])
            },
        }),
        stubs: ['el-input', 'el-form', 'el-form-item', 'el-button', 'el-select', 'el-option',
            'el-input-number', 'el-date-picker', 'el-switch', 'el-icon', 'el-divider',
            'el-table', 'el-table-column', 'el-checkbox-group', 'el-checkbox', 'el-tag',
            'el-tabs', 'el-tab-pane', 'el-dropdown', 'el-dropdown-menu', 'el-dropdown-item',
            'el-dialog', 'el-drawer', 'el-popover', 'el-pagination', 'el-tree', 'el-empty'],
    }
}

function walk(n, out = []) { out.push(n); (n.children || []).forEach(c => walk(c, out)); return out }

async function checkRender() {
    const { Vue, sandbox, loadComponent } = loadRuntime()
    const { renderer, node, stub, stubs } = makeRenderer(Vue)
    let failed = 0
    const fail = (msg) => { failed++; console.log('  FAIL ' + msg) }
    const pass = (msg) => console.log('  ok   ' + msg)

    // 行结构（.fr-item > .fr-label + 控件）— 字段行与节点表单里手写的 display 行必须一致
    const shapeOf = (row) => {
        const label = (row.children || []).find(c => c.props && c.props.class === 'fr-label') || { children: [] }
        return {
            classes: row.props.class,
            spanClasses: label.children.filter(c => c.tag === 'span').map(c => c.props.class || ''),
            texts: label.children.filter(c => c.tag === 'span').map(c => c.text || ''),
            // 控件由 kind 声明的渲染器承担（调度器只放 <component :is>）→ 整棵子树找
            control: (walk(row).map(n => n.tag).filter(t => t && /^(el-|widget-)/.test(t))[0]) || '', 
        }
    }
    // 渲染期抛错必须让用例失败 —— 不然组件在浏览器里报错, 这里照样是绿的。
    // kind 名 → 组件：断言里看得见名字（真组件是异步的，树里只到 <component> 一层）
    sandbox.Widgets = {
        resolve: (kind) => (kind ? stub('widget-' + kind) : null),
        truncate: (v) => String(v === undefined || v === null ? '' : v),
        plain: (v) => String(v || ''), localTime: (v) => String(v || ''),
        localDate: (v) => String(v || ''), fileName: (v) => String(v || ''), isImage: () => false,
    }
    const renderErrors = []
    const mount = (comp, props, components) => {
        const host = node('#root')
        const app = renderer.createApp(comp, props)
        // 与 js/panel.js 一致：模板里的全局要挂到 globalProperties（否则 _ctx.Widgets 是 undefined）
        if (sandbox.Widgets) app.config.globalProperties.Widgets = sandbox.Widgets
        app.config.errorHandler = (err) => { renderErrors.push(err && err.message ? err.message : String(err)) }
        components.forEach(name => app.component(name, stub(name)))
        app.mount(host)
        return host
    }
    const noReq = (s) => JSON.stringify(s.spanClasses.filter(c => c !== 'fr-req'))
    const printRow = (tag, s) => console.log('       ' + tag + ': ' + JSON.stringify(s))

    // ① FieldRenderer 渲染字段行（含嵌套 object，不应多出行来）
    const FieldRenderer = await loadComponent('/pages/FieldRenderer.vue')
    const fields = [
        { name: 'title', kind: 'text', label: '标题', required: true },
        { name: 'position', kind: 'number', label: '排序' },
        { name: 'meta', kind: 'object', label: '元信息', fields: [{ name: 'note', kind: 'text', label: '备注' }] },
    ]
    const fieldHost = mount(FieldRenderer.default || FieldRenderer,
        { fields, modelValue: { title: 'T', position: 1, meta: { note: 'N' } } }, stubs)
    const fieldRows = walk(fieldHost).filter(n => n.tag === 'div' && n.props.class === 'fr-item').map(shapeOf)
    fieldRows.forEach((s, i) => printRow('字段行' + i, s))
    if (fieldRows.length !== 4) fail('字段行数 = ' + fieldRows.length + '（期望 3 字段 + 1 嵌套）')
    else pass('字段行按 schema 渲染（含嵌套 object）')

    // 只读字段（editable 之外）: 必须走 view 模式 —— cell 是列表用的紧凑形态,
    // 在表单里会把长文本/富文本截断（用户看不到全文）。掩码字段走占位行, 两者不同。
    const readonlySrc = read(path.join(ADMIN_DIR, 'pages/FieldRenderer.vue'))
    if (!/isReadOnly\(f\)[^>]*\n?[^>]*mode="view"/.test(readonlySrc) && !/mode="view"/.test(readonlySrc)) {
        fail('只读字段没有走 view 模式（列表用的 cell 会截断长文本）')
    }
    // 只读的值要能选中复制、引用链接要能点开 ⇒ 不能 pointer-events: none
    if (/\.fr-readonly\s*\{[^}]*pointer-events\s*:\s*none/.test(readonlySrc)) {
        fail('只读行写了 pointer-events: none —— 值不能选中复制、引用链点不开')
    }

    // ② NodeEditDialog 里的 display 行（手写）必须与字段行同构
    const NodeEditDialog = await loadComponent('/pages/NodeEditDialog.vue')
    const dialogHost = mount(NodeEditDialog.default || NodeEditDialog, {
        visible: true, isEdit: false, typeName: 'article',
        defs: { article: { fields } },
    }, stubs.concat(['el-drawer']))
    // ②a 三字段驱动表单: masked 的字段根本不渲染; 不可写的字段只读（走组件的 cell 模式,
    //     不是输入框）; 可写的字段仍是编辑控件。FieldRenderer 是干这件事的地方, 直接挂它。
    const facts = [
        { name: 'title', kind: 'text', label: '标题' },      // 可写
        { name: 'body', kind: 'richtext', label: '正文' },    // 不可写（可写集合里没有它）
        { name: 'secret', kind: 'text', label: '内部备注' },   // 被读规则裁掉（masked）
    ]
    const factsHost = mount(FieldRenderer.default || FieldRenderer, {
        fields: facts, modelValue: { title: '标题', body: '正文' },
        editing: true, masked: ['secret'], readonly: ['body'],
    }, stubs)
    // 只读行会多带一个 fr-readonly 类 ⇒ 用包含判断（=== 会把它们漏掉）
    const factsRows = walk(factsHost).filter(n => n.tag === 'div' &&
        String(n.props.class || '').split(/\s+/).indexOf('fr-item') >= 0)
    // 文本可能落在 #text 子节点, 也可能被 setElementText 写在元素上 ⇒ 两处都看
    const textsOf = (r) => walk(r).map(n => n.text).filter(t => t !== undefined && t !== null && t !== '')
    const rowOf = (name) => factsRows.find(r => textsOf(r).some(t => String(t).trim() === name))
    const isWidget = (n) => String(n.tag).indexOf('widget-') === 0
    const labels = factsRows.map(r => textsOf(r).join('|')).join('  //  ')
    // masked 的字段: **不显示值**, 但要留一行明确的占位（渲染成空会被当成"没值"）
    const maskedRow = rowOf('内部备注')
    const maskedTexts = maskedRow ? textsOf(maskedRow).join('|') : ''
    if (factsRows.length !== 3) {
        fail('三字段表单应画 3 行（masked 那行是占位）, 实际 ' + factsRows.length + ' 行: ' + labels)
    }
    else if (!maskedRow) fail('被读规则裁掉的字段（masked）连占位行都没有: ' + labels)
    else if (maskedTexts.indexOf('无权限查看') < 0) {
        fail('masked 行没写"无权限查看"占位: ' + maskedTexts)
    }
    else if (walk(maskedRow).some(isWidget) || walk(maskedRow).some(n => n.tag === 'el-input')) {
        fail('masked 行不该有输入控件（值看不到, 别给编辑入口）')
    }
    else if (!rowOf('正文')) fail('找不到正文那一行: ' + labels)
    else if (walk(rowOf('正文')).some(n => n.tag === 'el-input')) {
        fail('不可写的字段渲染成了输入框（应只读展示）')
    } else if (!walk(rowOf('正文')).some(isWidget)) {
        fail('不可写的字段没有走组件渲染: ' + walk(rowOf('正文')).map(n => n.tag).join(','))
    } else if (!walk(rowOf('标题')).some(isWidget)) {
        fail('可写的字段没有渲染编辑控件')
    } else {
        pass('masked 字段留"无权限查看"占位（无控件）；不可写字段走组件只读展示；可写字段仍是编辑控件')
    }

    // display 系统列已经去掉（显示什么完全由 types 声明决定）⇒ 节点表单里**不该**再手写
    // 字段行: 所有行都来自 FieldRenderer。这里查源码而不是 DOM —— mount 时 watcher 还没跑,
    // def 为空 ⇒ DOM 里 0 行, 断言 DOM 只会绿灯放行"手写行又回来了"。
    const dialogSrc = read(path.join(ADMIN_DIR, 'pages/NodeEditDialog.vue'))
    const handRows = (dialogSrc.match(/class="fr-item/g) || []).length
    if (handRows > 0) {
        fail('NodeEditDialog 里又手写了 ' + handRows + ' 个 .fr-item 行（字段该全部交给 FieldRenderer）')
    } else {
        pass('NodeEditDialog 没有手写字段行（全部由 FieldRenderer 渲染）')
    }

    // 权限矩阵页不在这里挂: 它的结构（el-tabs + 表格列 v-for + 列作用域槽）会踩到本渲染器
    // 桩件的空缺 —— 空数据挂也一样崩（"S is not a function or its return value is not iterable"）,
    // 定位到的问题在桩件侧, 不在页面逻辑。这条闸门只覆盖编译（checkSFC）+ 下面的渲染项。
    // 真要覆盖它得换成真 DOM（vendor 里的 Vue + jsdom 之类）, 属于闸门基建的下一轮。

    // ③ 同上的组件在 nodes.vue 里的真实用法: :is-edit 恒为 true, 而 node 在点开某一行之前是 null。
    renderErrors.length = 0
    mount(NodeEditDialog.default || NodeEditDialog, {
        visible: false, isEdit: true, node: null, typeName: 'article', defs: { article: { fields } },
    }, stubs.concat(['el-drawer']))
    if (renderErrors.length) fail('NodeEditDialog 在 node=null + isEdit=true 下渲染报错: ' + renderErrors[0])
    else pass('NodeEditDialog 在 node=null + isEdit=true 下不报错（nodes.vue 的真实用法）')

    // ③b FieldRenderer 的引用预载：首次打开下拉拉一批；之后（包括用户搜过之后）不再拉 ——
    //     否则打字搜出来的几条会在下次打开时被预载结果覆盖掉。
    const FR = await loadComponent('/pages/FieldRenderer.vue')
    const frMethods = (FR.default || FR).methods
    // ③a kind → 渲染器：契约在 Go（types.Kind.Render），前端只认渲染器名。
    //     这里钉住三件事：渲染器文件齐全、调度器不认识 kind、列表走渲染器（不裸输出）。
    const widgetsSrc = read(path.join(ADMIN_DIR, 'js/widgets.js'))
    if (/var\s+KNOWN|KNOWN\s*=|\bWHITELIST\b/.test(widgetsSrc)) {
        fail('js/widgets.js 里出现了组件名清单：kind 名本身就是组件名，前端不该维护任何清单')
    } else if (!/'widgets\/'\s*\+/.test(widgetsSrc)) {
        fail('js/widgets.js 没有按名字拼组件路径（应 ' + "'widgets/' + kind + '.vue'" + '）')
    } else if (!/errorComponent/.test(widgetsSrc)) {
        fail('js/widgets.js 缺 errorComponent：组件文件缺失会静默渲染空白（要 fail-loud）')
    } else {
        pass('组件解析只按 kind 名取文件，没有清单；取不到就显示错误块')
    }

    // 调度器（FieldRenderer）不认识 kind：只有 array / object 是结构，其余走 kind 的 Render()
    const frSrc = read(path.join(ADMIN_DIR, 'pages/FieldRenderer.vue'))
    const kindNames = new Set()
    for (const m of frSrc.matchAll(/kind\s*===\s*['"]([a-z\[\]-]+)['"]/g)) kindNames.add(m[1])
    const illegal = [...kindNames].filter(k => k !== 'array' && k !== 'object')
    if (illegal.length) fail('FieldRenderer 里出现了具体 kind 名（只该认两处**结构**: array/object）: ' + illegal.join(' '))
    else pass('FieldRenderer 只处理 array/object 结构，叶值全部按 field.kind 取组件')

    const nodesSrc = read(path.join(ADMIN_DIR, 'pages/nodes.vue'))
    const problems = []
    if (!/Widgets\.resolve\(/.test(nodesSrc)) problems.push('列表没有通过 Widgets.resolve 取渲染器')
    if (/:is="cellOf\(/.test(nodesSrc) === false) problems.push('列表单元格没有用渲染器组件')
    if (/fieldValue/.test(nodesSrc)) problems.push('列表里还留着旧的 fieldValue 裸输出')
    if (!/@open-node=/.test(nodesSrc)) problems.push('列表没有接住引用的 @open-node（点引用链接没反应）')
    if (!/refEdit\.visible/.test(nodesSrc)) problems.push('缺引用目标的编辑抽屉（点引用链接打不开表单）')
    // ref/refs 是**筛选**语义（单选 vs 多选 → 查询 AST 不同），不是渲染特判，允许出现
    const kindLit = /kind\s*===\s*[\x22\x27](timestamp|upload-image|upload-file|gallery|richtext|bool|select|address|textarea|number|text)[\x22\x27]/
    if (kindLit.test(nodesSrc)) {
        problems.push('列表里按 kind 名特判显示（应交给同名组件）')
    }
    // 单元格组件必须拿到 defs: 引用列靠它把引用目标解析成名字（缺了就只能显示 #id）。
    // 真实踩过: 列表里每个 ref 列都显示 "#61 #61", 而编辑表单（FieldRenderer）正常 ——
    // 因为那边传了 defs, 这里没传。
    const cellTags = nodesSrc.match(/<component[^>]*:is="cellOf\([^>]*>/g) || []
    if (cellTags.length === 0) {
        problems.push('列表里找不到单元格组件')
    }
    for (const tag of cellTags) {
        if (!/:defs=/.test(tag)) {
            problems.push('列表单元格没传 :defs（引用列会退化成 #id）: ' + tag.replace(/\s+/g, ' ').slice(0, 90))
        }
    }
    for (const problem of problems) fail(problem)
    if (!problems.length) pass('列表单元格走渲染器，没有 kind 特判')

    const nodeCalls = []
    sandbox.$api = {
        refLabel: (n, def) => {
            if (!n) return '#?'
            if (n.label) return n.label
            for (const c of (((def || {}).admin || {}).columns || [])) {
                const v = (n.fields || {})[c]
                if (typeof v === 'string' && v.trim()) return v
            }
            return '#' + n.id
        },
        nodes: async (type, params) => { nodeCalls.push({ type, params }); return { items: [] } },
    }
    const RefWidget = await loadComponent('/widgets/ref.vue')
    const wMethods = (RefWidget.default || RefWidget).methods
    const fctx = { found: [], loading: false, loaded: false, node: { expand: {} }, defs: {},
        field: { name: 'category', to: 'category', kind: 'ref' }, $emit: () => {} }
    for (const name of Object.keys(wMethods)) fctx[name] = wMethods[name].bind(fctx)
    await fctx.preload()
    await fctx.preload()
    console.log('       引用控件调用: ' + JSON.stringify(nodeCalls))
    if (nodeCalls.length !== 1) {
        fail('引用候选只该拉一次（打开下拉时）, 实际 ' + nodeCalls.length + ' 次')
    } else if (nodeCalls[0].type !== 'category' || nodeCalls[0].params.size !== 100 ||
        nodeCalls[0].params.sort !== '-id') {
        fail('引用候选该拉目标类型的一页（size=100, sort=-id）: ' + JSON.stringify(nodeCalls[0]))
    } else if (typeof fctx.search === 'function' || typeof fctx.load === 'undefined') {
        fail('引用控件还在走远程搜索（gcmv3 没有检索端点: 拉一页 + 组件本地过滤）')
    } else {
        pass('引用候选: 首次打开拉一页、之后不重复（过滤交给组件的 filterable）')
    }

    // ④ nodes.vue 引用筛选: 每个 ref 字段一项, 选中/清除 + 多字段 AND 组合。
    //    （树视图与树筛选已延后 —— 内核没有 tree, admin.view=tree 暂时只是声明。）
    const NodesPage = await loadComponent('/pages/nodes.vue')
    const nodesComp = NodesPage.default || NodesPage
    const NodesPageMethods = nodesComp.methods

    const methods = nodesComp.methods
    // ④b 点引用链接 → 打开目标节点的编辑抽屉（只带 id+type，表单自己去拉全量与引用回显）
    const refCtx = { ...nodesComp.data(), $emit: () => {} }
    methods.openRef.call(refCtx, { id: 42, type: 'industry', label: '钢铁' })
    const opened = refCtx.refEdit || {}
    if (!opened.visible || !opened.node || opened.node.id !== 42 || opened.typeName !== 'industry') {
        fail('点引用链接没有打开目标节点: ' + JSON.stringify(opened))
    } else if (opened.node.type !== 'industry') {
        fail('引用节点缺 type: 表单按目标类型取字段定义，缺了就画不出来')
    } else {
        pass('点引用链接: 用目标 id+type 打开编辑抽屉（类型跟着目标走）')
    }
    const baseData = typeof nodesComp.data === 'function' ? nodesComp.data() : {}
    const fake = {
        ...baseData,
        query: { ...(baseData.query || {}), page: 7, filter: '' },
        $refs: { 'fp-category': [{ hide() {} }] },
        combineFilters: methods.combineFilters,
        applyFilters: methods.applyFilters,
        titleOf: () => '新闻',
        refresh() {},
    }
    // 组件的方法统统绑到假上下文上（只补没显式覆盖的）—— 免得每加一个方法就得回来补测试脚手架。
    for (const name of Object.keys(methods)) {
        if (fake[name] === undefined) fake[name] = methods[name].bind(fake)
    }
    // 假上下文里用到的 data 字段, 必须在组件自己的 data() 里真实存在 ——
    // 否则测试自己造了一个组件里根本没有的字段, 页面在浏览器里炸了这里却是绿的。
    for (const key of Object.keys(fake)) {
        if (key.startsWith('$') || typeof fake[key] === 'function') continue
        if (!(key in baseData)) throw new Error('nodes.vue 的 data() 缺少 ' + key + '（data() 被改坏了?）')
    }
    const fr = { field: 'event', to: 'event', active: 0, activeLabel: '', _ids: null,
                 options: [{ id: 83, label: '2026 新能源产业对接会 #83' }] }
    const fc = { field: 'category', to: 'category', active: 0, activeLabel: '', _ids: null }
    fake.filters = [fc, fr]
    methods.pickRef.call(fake, fc, 9)
    const one = { filter: fake.query.filter, active: fc.active, page: fake.query.page }
    methods.pickRef.call(fake, fr, 83)
    const both = fake.query.filter
    methods.clearFilter.call(fake, fc)
    const cleared = { filter: fake.query.filter, active: fc.active, ids: fc._ids }
    console.log('       选分类: ' + JSON.stringify(one) + '\n       再选活动: ' + JSON.stringify(both)
        + '\n       清空: ' + JSON.stringify(cleared))
    if (one.filter !== '(in ->category [9])' || one.page !== 1) {
        fail('单选引用没有写过滤串/回到第 1 页: ' + JSON.stringify(one))
    } else if (both !== '(and (in ->category [9]) (in ->event [83]))') {
        fail('多字段引用筛选没有 AND 组合')
    } else if (cleared.filter !== '(in ->event [83])' || cleared.active !== 0 || cleared.ids !== null) {
        fail('清除一个筛选后另一个该留着: ' + JSON.stringify(cleared))
    } else {
        pass('引用筛选: 单选节点、多字段 AND、清除只影响自己')
    }

    // ④ 切换类型要清掉上一个类型的查询残留: 筛选表达式(filter)/页码
    fake.query.filter = '(in ->category [9])'
    fake.query.page = 7
    methods.selectType.call(fake, 'signup')
    if (fake.query.filter !== '' || fake.query.page !== 1) {
        fail('切换类型后查询残留: ' + JSON.stringify({ filter: fake.query.filter, page: fake.query.page }))
    } else {
        pass('切换类型清空筛选/页码')
    }

    // ⑤ 左侧类型列表: 组内按类型键排序；没填 group 的与站点自命的"未分组"并成同一节，且排最前
    const grouped = methods.buildTypeGroups.call({
        typeNames: ['article', 'banner', 'category', 'event', 'industry', 'member', 'page', 'supply'],
        typeDefs: {
            article: { admin: { label: '文章', group: '内容' } },
            banner: { admin: { group: '内容' } },                  // 没填 label → 回退类型键
            category: { admin: { label: '分类', group: '基础数据' } },
            event: { admin: { label: '活动' } },                   // 没填 group
            industry: { admin: { label: '行业', group: '基础数据' } },
            member: { admin: { label: '会员单位' } },              // 没填 group
            page: { admin: { label: '单页', group: '未分组' } },    // 站点自己就叫"未分组"
            supply: { admin: { label: '供需', group: '内容' } },
        },
    })
    const shape = grouped.map(g => g.name + ':' + g.items.map(i => i.label).join('/'))
    console.log('       ' + JSON.stringify(shape))
    const keys = (g) => grouped[g] ? grouped[g].items.map(i => i.name).join() : '(缺组)'
    if (grouped[0].name !== '未分组' || keys(0) !== 'event,member,page') {
        fail('"未分组"没有合并成一节并排在最前')
    } else if (grouped[1].name !== '内容' || keys(1) !== 'article,banner,supply') {
        fail('组内没有按类型键排序')
    } else if (grouped[2].name !== '基础数据' || keys(2) !== 'category,industry' || grouped.length !== 3) {
        fail('分组顺序/数量不对（应按首次出现）')
    } else if (grouped[1].items.find(i => i.name === 'banner').label !== 'banner') {
        fail('admin.label 缺省没有回退类型名')
    } else {
        pass('类型列表: 未分组合并且最前 + 组内按类型键 + label 回退')
    }
    // ⑨ 每个 kind 的**真实**组件（不是桩件）：cell 模式必须真的渲染出内容。
    //    组件是异步的 —— 等一拍再断言；"列表里那列是空的"就是这么漏掉的。
    vm.runInContext(read(path.join(ADMIN_DIR, 'js/widgets.js')), sandbox, { filename: 'widgets.js' })

    // ③c nodes.vue 的时间列格式化: 值现在是 **Unix 秒**（整数）——
    //     直接对数字做 .replace('T',' ') 会抛 TypeError（真实踩过, 列表整页崩）。
    const fmt = (NodesPageMethods || {}).fmt
    if (typeof fmt !== 'function') {
        fail('nodes.vue 没有 fmt（更新时间列的格式化）')
    } else {
        let formatted = ''
        try {
            formatted = fmt.call({}, 1789000000)
        } catch (err) {
            fail('时间列格式化吃不下 Unix 秒: ' + (err && err.message ? err.message : String(err)))
        }
        if (formatted && !/^\d{4}-\d{2}-\d{2}/.test(formatted)) {
            fail('时间列该显示成本地日期: ' + JSON.stringify(formatted))
        } else if (formatted && fmt.call({}, null) !== '') {
            fail('空值该显示成空串')
        } else if (formatted) {
            pass('列表时间列: Unix 秒 → 本地时间（' + formatted + '）')
        }
    }




    sandbox.Panel = { loadComponent: (rel) => loadComponent('/' + rel) }
    // 期望值：每行 [modelValue, 应该看到的内容注解]（文本片段或图片数）
    const samples = {
        text: '标题', textarea: '第一行\n第二行', address: 'about-us', richtext: '<p>正文</p>',
        number: 3, bool: true, select: 'draft', timestamp: 1784367000, // 2026-07-18T09:30:00Z（Unix 秒）
        'upload-image': '/uploads/a.png', 'upload-file': '/uploads/a.mp4',
        gallery: ['/uploads/a.png', '/uploads/b.png'], ref: 7, refs: [7, 8],
    }
    const expect = {
        text: { text: '标题' }, textarea: { text: '第一行' }, address: { text: 'about-us' },
        richtext: { text: '正文' }, number: { text: '3' }, bool: { text: '✓' },
        select: { text: 'draft' }, timestamp: { text: '2026' },
        'upload-image': { imgs: 1, src: '/uploads/a.png' }, 'upload-file': { text: 'a.mp4' },
        // 引用单元格必须"显示名 + #id"：少了 #id 同名节点就分不出来（曾经把标签简化掉过一次）
        gallery: { imgs: 2 }, ref: { text: '引用目标', link: true, hash: '#1' },
        refs: { text: '引用目标', link: true, hash: '#1' },
    }
    const tick = () => new Promise(r => setTimeout(r, 50))
    for (const kind of Object.keys(samples)) {
        renderErrors.length = 0
        const host = mount({
            render() {
                return Vue.h(sandbox.Widgets.resolve(kind), {
                    mode: 'cell', modelValue: samples[kind], field: { kind, name: 'ref' },
                    // 引用目标从 node.expand 取（组件接口：node + field + mode）。
                    // 显示名走 refLabel: 这里给真实的形状（fields + defs.admin.columns）——
                    // 没有 display 系统列了。
                    node: { expand: { ref: [{ id: 1, type: 'category', fields: { name: '引用目标' } }] } },
                    defs: { category: { admin: { columns: ['name'] }, fields: [{ name: 'name', kind: 'text' }] } },
                })
            },
        }, {}, stubs)
        await tick()
        const nodes = walk(host)
        const texts = nodes.map(n => n.text || '').join('')
        const imgs = nodes.filter(n => n.tag === 'img')
        const want = expect[kind] || {}
        if (renderErrors.length) {
            fail(kind + ' 的 cell 渲染抛错: ' + renderErrors.join(' / '))
        } else if (/widget-missing/.test(texts)) {
            fail(kind + ' 的 cell 显示「缺组件」错误块: ' + texts)
        } else if (want.text && texts.indexOf(want.text) < 0) {
            fail(kind + ' 的 cell 没显示出值: 期望含 ' + JSON.stringify(want.text) +
                '，实际 ' + JSON.stringify(texts) + '（tree=' + JSON.stringify(nodes.map(n => n.tag)) + '）')
        } else if (want.link && !nodes.some(n => n.tag === 'a' && n.props.class === 'w-ref-link')) {
            fail(kind + ' 的 cell 不是链接（引用应该能点开目标节点的编辑表单）')
        } else if (want.hash && texts.indexOf(want.hash) < 0) {
            fail(kind + ' 的 cell 少了 #id 后缀（引用标签必须是 显示名 + #id）: ' + JSON.stringify(texts))
        } else if (want.imgs && imgs.length !== want.imgs) {
            fail(kind + ' 的 cell 图片数 = ' + imgs.length + '，期望 ' + want.imgs)
        } else if (want.src && !imgs.some(n => n.props.src === want.src)) {
            fail(kind + ' 的 cell 图片 src 不对: ' + JSON.stringify(imgs.map(n => n.props.src)))
        }
    }
    pass('13 个真实组件在 cell 模式下渲染出了正确的值（文本/图片）')

    // ⑨b **只读详情（view）不能截断**: cell 是列表用的紧凑形态, 表单里的只读字段
    //     必须看全（真实问题: 富文本/多行只读时被压成一行截断, 用户看不到全文）。
    renderErrors.length = 0
    const longText = 'x'.repeat(200) + '结尾'
    for (const kind of ['text', 'textarea', 'richtext']) {
        const host = mount({
            render() {
                return Vue.h(sandbox.Widgets.resolve(kind), {
                    mode: 'view', modelValue: longText, field: { kind, name: 'body' }, defs: {},
                })
            },
        }, {}, stubs)
        await tick()
        // 富文本走 v-html ⇒ 内容在 innerHTML 上（不在 text 节点里）
        const shown = walk(host).map(n => (n.text || '') + ((n.props && n.props.innerHTML) || '')).join('')
        if (shown.indexOf('结尾') < 0) {
            fail(kind + ' 的只读详情把内容截断了（mode=view 必须看全）: ' + JSON.stringify(shown.slice(0, 40)))
        } else if (!shown) {
            fail(kind + ' 的只读详情什么都没渲染')
        }
    }
    if (renderErrors.length) {
        fail('只读详情渲染报错: ' + renderErrors.join(' / '))
    } else {
        pass('只读详情（view）: 文本/多行/富文本都显示完整内容（不截断）')
    }
    // ⑩ 编辑器链路（穿透异步组件）：真实组件里改值 → 表单收到 update:modelValue。
    //    监听器要穿过 defineAsyncComponent 才到得了 FieldRenderer —— 这里断了，
    //    编辑会"看着能打字、保存却是空值"，没有任何报错。
    renderErrors.length = 0
    let edited = null
    const editorHost = mount(FieldRenderer.default || FieldRenderer, {
        fields: [{ name: 'title', kind: 'text', label: '标题' }],
        modelValue: { title: '旧值' },
        'onUpdate:modelValue': (v) => { edited = v },
    }, stubs)
    await tick()
    const input = walk(editorHost).find(n => n.tag === 'el-input')
    if (!input) fail('编辑表单里没有找到输入控件（真实组件没加载出来？）')
    else if (typeof input.props['onUpdate:modelValue'] !== 'function') {
        fail('输入控件的 update:model-value 监听器没传下来（改值不会回流到表单）')
    } else {
        input.props['onUpdate:modelValue']('新值')
        if (!edited || edited.title !== '新值') {
            fail('组件里改值没有回流到表单: ' + JSON.stringify(edited))
        } else {
            pass('编辑器链路通: 真实组件改值 → FieldRenderer 收到 update:modelValue')
        }
    }
    // ⑪ 空值归一不算修改：控件初始化会把"字段不存在"归一成 null/''/[]，照单全收就会
    //    出现"点开什么都不动、关闭也问要不要保存"（真实踩过：article 没有 publish_time，
    //    el-date-picker 空值 → toCanonical(null) = null → 表单从 {} 变成 {publish_time:null}）。
    renderErrors.length = 0
    let blankEdited = 0
    const blankHost = mount(FieldRenderer.default || FieldRenderer, {
        fields: [{ name: 'publish_time', kind: 'timestamp' }, { name: 'title', kind: 'text' }],
        modelValue: {},
        'onUpdate:modelValue': () => { blankEdited++ },
    }, stubs)
    await tick()
    const picker = walk(blankHost).find(n => n.tag === 'el-date-picker')
    if (!picker) {
        fail('编辑表单里没找到时间控件')
    } else if (typeof picker.props['onUpdate:modelValue'] !== 'function') {
        fail('时间控件的 update:model-value 没传下来')
    } else {
        picker.props['onUpdate:modelValue'](null)   // 控件把空值归一（初始化时就会发生）
        if (blankEdited) {
            fail('空值归一被当成用户修改了（点开不动也会提示"有未保存修改"）')
        } else {
            picker.props['onUpdate:modelValue'](new Date('2026-01-02T03:04:05Z'))
            if (blankEdited !== 1) {
                fail('真改了时间却没回流到表单: ' + blankEdited)
            } else {
                pass('空值归一不算修改；真改值才回流')
            }
        }
    }
    // ⑫ 老格式的时间值不许被静默清空：库里可能是纪元数字/老格式（迁移前），
    //    选择器读不出来会 emit null —— 回写就等于把数据删了，还顺带误判"有未保存修改"。
    renderErrors.length = 0
    async function timestampEmit(modelValue) {
        let emitted = 'none'
        const host = mount(sandbox.Widgets.resolve('timestamp'), {
            mode: 'edit', modelValue: modelValue,
            field: { kind: 'timestamp', name: 'publish_time' }, defs: {},
            'onUpdate:modelValue': (v) => { emitted = v },
        }, stubs)
        await tick()
        const picker = walk(host).find(n => n.tag === 'el-date-picker')
        if (!picker || typeof picker.props['onUpdate:modelValue'] !== 'function') {
            fail('时间控件（edit 模式）没渲染出可用的选择器')
            return { emitted: emitted, texts: '' }
        }
        picker.props['onUpdate:modelValue'](null)   // 选择器把解析不了的值归一成空
        return { emitted: emitted, texts: walk(host).map(n => n.text || '').join('') }
    }
    // v2 的 ISO 字符串（旧库）与毫秒值: 选择器读不出来 ⇒ 不许回写（否则静默清数据）+ 要提示
    for (const bad of ['2026-01-02T03:04:05Z', 1790179200000]) {
        const legacy = await timestampEmit(bad)
        if (legacy.emitted !== 'none') {
            fail(bad + ' 被控件回写成 ' + JSON.stringify(legacy.emitted) + ' —— 静默清空数据')
        } else if (legacy.texts.indexOf('不是整数秒') < 0) {
            fail(bad + ' 没有提示出来（用户不知道值不是整数秒）: ' + JSON.stringify(legacy.texts))
        } else {
            pass('非整数秒的值（' + bad + '）: 不回写 + 有提示')
        }
    }
    const clearing = await timestampEmit(1784367000)   // 正常值（Unix 秒）
    if (clearing.emitted !== null) {
        fail('清空一个正常的时间值必须能回写 null（否则用户没法清空）: ' + JSON.stringify(clearing.emitted))
    } else {
        pass('正常时间值（Unix 秒）可以清空（回写 null）')
    }
    // ⑬ array 是"结构"（没有组件文件），由 FieldRenderer 自己递归渲染：条目 + 上移/下移/
    //    删除 + 「+ 添加一项」必须都在，子字段（object）也要递归出来。
    renderErrors.length = 0
    let arrEdited = null
    const arrayHost = mount(FieldRenderer.default || FieldRenderer, {
        // 用真实站点的形状（viicn slide）：4 条，image 是必填 upload-image 且值为空串。
        fields: [{
            name: 'slides', kind: 'array', label: '轮播图',
            item: { kind: 'object', fields: [
                { name: 'image', kind: 'upload-image', required: true, label: '图片' },
                { name: 'h1', kind: 'text', label: '主标题' },
            ] },
        }],
        modelValue: { slides: [
            { image: '', h1: '深度战略合作' }, { image: '', h1: '战略研究驱动增长' },
            { image: '', h1: '品牌点亮城市' }, { image: '', h1: '客户案例' },
        ] },
        'onUpdate:modelValue': (v) => { arrEdited = v },
    }, stubs)
    await tick()
    const arrNodes = walk(arrayHost)
    const arrTexts = arrNodes.map(n => n.text || '').join('|')
    const arrTags = arrNodes.map(n => n.tag).join(',')
    if (renderErrors.length) {
        fail('array 渲染抛错: ' + renderErrors.join(' / '))
    } else if (arrTexts.indexOf('#1') < 0) {
        fail('array 没渲染出条目（应显示 #1）: tag=' + arrTags + ' text=' + JSON.stringify(arrTexts))
    } else if (arrTexts.indexOf('添加一项') < 0) {
        fail('array 没渲染出「+ 添加一项」按钮: ' + JSON.stringify(arrTexts))
    } else if (arrTexts.indexOf('主标题') < 0) {
        fail('array 的 object 子字段没递归渲染（缺「主标题」）: ' + JSON.stringify(arrTexts))
    } else {
        // 按钮的文字在子节点上（stub 里 <el-button><span>+ 添加一项</span></el-button>），
        // 所以要按"子树文本"找按钮，不能只看按钮自己的 text。
        const subText = (n) => walk(n).map(c => c.text || '').join('')
        const addBtn = arrNodes.find(n => n.tag === 'el-button' && subText(n).indexOf('添加一项') >= 0)
        const addClick = addBtn && (addBtn.props.onClick || addBtn.props['on-click'])
        if (addBtn && !addClick) fail('添加按钮上挂的点击事件名不认识: ' + JSON.stringify(Object.keys(addBtn.props || {})))
        if (!addBtn || typeof addClick !== 'function') {
            fail('「+ 添加一项」按钮没有点击回调: ' + JSON.stringify(addBtn && Object.keys(addBtn.props || {})))
        } else {
            addClick()
            await tick()
            if (!arrEdited || !Array.isArray(arrEdited.slides) || arrEdited.slides.length !== 5) {
                fail('点「+ 添加一项」没有往表单里加条目: ' + JSON.stringify(arrEdited))
            } else {
                pass('array 结构渲染完整（条目/子字段/添加按钮，加了能回流）')
            }
        }
    }
    // ⑭ App.vue 必须真的 mount 得起来 —— 浏览器里 setup 抛错就是白屏。
    //
    // 静态检查（checkAppReturns）挡得住"return 了没定义的名字", 挡不住其它 setup 期抛错
    // （少个 import、拼错 API、解构错）。真实踩过: 删旧功能时把 var globalQ 一起删了 ——
    // 页面白屏, 而当时所有闸门都是绿的。
    //
    // 环境按 index.html 的真实样子补齐: AppConfig（菜单壳子）、Panel（组件加载器）、
    // $api（异步的, 用最小 stub 返回形状对的数据）。
    sandbox.AppConfig = {
        defaultPage: 'dashboard',
        menu: [{ key: 'nodes', label: '内容管理', icon: 'Document', route: 'nodes', group: '平台' }],
    }
    sandbox.Panel = {
        loadComponent: (rel) => loadComponent('/' + rel),
        onError: () => {},                    // App.vue 在这里注册 401 → 登出
        get: async () => ({}), post: async () => ({}),
    }
    // 注意: SFC 加载器在 import 时就把 `$api` **快照**下来了（import $api from '$api'），
    // 之后再 Object.assign 补方法**不会**被组件看见（踩过: 登录渠道那段就是这么静默失效的）。
    // 所以这里一次给全, 行为用开关切（未登录 / 已登录两种场景挂两次）。
    let anonymous = false
    sandbox.$api = {
        me: async () => {
            if (anonymous) throw new Error('未登录')
            return { actor: { node_id: 1, node_type: 'staff', realm: 'staff', roles: ['owner'] },
                node: { id: 1, type: 'staff', fields: { name: '站长' } } }
        },
        types: async () => ({ types: {} }),
        loginRealms: async () => ({ realms: [
            { name: 'member', node_type: 'member', default: true },
            { name: 'staff', node_type: 'staff' },
        ] }),
        get: async () => ({ items: [] }),
        post: async () => ({}),
        logout: async () => ({}),
        refLabel: (n) => (n && n.fields && n.fields.name) || '#1',
    }
    renderErrors.length = 0
    const App = await loadComponent('/pages/App.vue')
    try {
        // setup 里抛的错在 Vue 里是**直接冒出来**的（不走 errorHandler）, 所以这里要接住,
        // 否则闸门以异常结束 —— 也是失败, 但看不见"为什么失败"。
        mount(App.default || App, {}, stubs)
        await tick()
    } catch (err) {
        renderErrors.push(err && err.message ? err.message : String(err))
    }
    if (renderErrors.length) {
        fail('App.vue 挂载报错（浏览器里就是白屏）: ' + renderErrors.join(' / '))
    } else {
        pass('App.vue 能挂载（setup 与模板都不抛错）')
    }

    // ⑮ 登录页的**渠道下拉**: 未认证时渲染, 每个选项都要有 label/value。
    //
    // 真实踩过: 模板里还写着 v2 的 `r.realm`, 而后端返回的是 `name` ⇒ 选项
    // label/value 全是 undefined ⇒ 下拉一片空白（而且选中值也对不上任何选项）。
    anonymous = true   // 切到"未登录": me() 失败 ⇒ 渲染登录页
    renderErrors.length = 0
    let loginHost
    try {
        loginHost = mount(App.default || App, {}, stubs)
        // 登录页要等两段异步: me() 拒 → phase=login → 再拉渠道清单
        await tick(); await tick(); await tick()
    } catch (err) {
        renderErrors.push(err && err.message ? err.message : String(err))
    }
    const realmOptions = loginHost ? walk(loginHost).filter(n => n.tag === 'el-option') : []
    const realmLabels = realmOptions.map(o => String(o.props.label === undefined ? '' : o.props.label))
    if (renderErrors.length) {
        fail('登录页挂载报错: ' + renderErrors.join(' / '))
    } else if (realmOptions.length !== 2) {
        fail('登录渠道下拉该有 2 个选项, 实际 ' + realmOptions.length)
    } else if (realmLabels.some(l => !l || l === 'undefined')) {
        fail('登录渠道选项没有 label（字段名对不上? 后端返回的是 name / node_type / default）: ' +
            JSON.stringify(realmOptions.map(o => o.props)))
    } else if (realmOptions.some(o => o.props.value === undefined)) {
        fail('登录渠道选项没有 value: ' + JSON.stringify(realmOptions.map(o => o.props)))
    } else {
        pass('登录渠道下拉: ' + realmLabels.join(' / '))
    }

    // ⑯ 引用标签（**真实实现 + 服务端真实形状**）: expand 里是完整节点 ⇒ 必须显示名字。
    //    真实踩过: 引用列显示成 "#61 #61"（refLabel 兜底 `#id` + refOption 又缀了 ` #61`）。
    //    这条放在最后: 它加载真实的 js/api.js（会覆盖前面的 $api 桩）。
    vm.runInContext(read(path.join(ADMIN_DIR, 'js/api.js')), sandbox, { filename: 'api.js' })
    // ① columns 第一列就是显示名时（常见）: 取它
    const realLabel = sandbox.$api.refLabel(
        { id: 61, type: 'category',
          fields: { address: 'about', name: '我会简介', position: 4 } },
        { admin: { columns: ['name', 'address'] }, fields: [{ name: 'name', kind: 'text' }] })
    // ② **声明了 admin.display** 时它优先: 这里 columns 第一列是 address（不是显示名）,
    //    没这条就会显示成 "about #61" —— 真实站点正是靠 display 才拿到"我会简介"。
    const declaredLabel = sandbox.$api.refLabel(
        { id: 61, type: 'category',
          fields: { address: 'about', name: '我会简介', position: 4 } },
        { admin: { display: 'name', columns: ['address', 'position'] },
          fields: [{ name: 'name', kind: 'text' }, { name: 'address', kind: 'address' }] })
    if (declaredLabel !== '我会简介') {
        fail('admin.display 该优先于 admin.columns, 实际 ' + JSON.stringify(declaredLabel))
    } else if (realLabel !== '我会简介') {
        fail('引用标签该取 admin.columns 里的 name, 实际 ' + JSON.stringify(realLabel) +
            '（引用列只显示 #id 就是这个兜底被触发了）')
    } else {
        pass('引用标签: display 优先（' + declaredLabel + '）、columns 兜底（' + realLabel + '）')
    }


    return failed
}

// ── 入口 ────────────────────────────────────────────────────────────
async function main() {
    const mode = process.argv[2] || 'sfc'
    if (mode !== 'sfc' && mode !== 'render') {
        console.error('用法: node web/admin/_tools/check.js [sfc|render]')
        process.exit(2)
    }

    console.log(mode === 'sfc' ? '编译校验 (' + pageList().length + ' 个页面)' : '渲染回归')
    // 每项闸门返回失败条数；顺序 = 输出顺序。加总后决定退出码 —— 漏掉哪一项，
    // 那个闸门就只是"打印了一行 FAIL"却不让命令失败（等于没写）。
    const parts = [checkAssets(), checkDestructiveWording(), checkDrawerClose(),
        checkTimestampWidget(), checkWidgetStyles(),
        checkWidgetInterface(), checkEditorRefLabels(), checkNoNestedTemplate()]
    const failed = parts.reduce((a, b) => a + b, 0) +
        (mode === 'sfc' ? (checkAppReturns() + await checkSFC()) : await checkRender())
    console.log(failed ? failed + ' 项失败' : '全部通过')
    process.exit(failed ? 1 : 0)
}

main().catch((err) => { console.error(err); process.exit(1) })
