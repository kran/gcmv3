<template>
  <div class="nodes-page">
    <!-- 左侧 type 列表（类型定义驱动, 动态） -->
    <div class="nodes-tree">
      <!--<div class="nodes-tree-header">
        <span style="font-size:14px;">类型</span>
        <el-button link type="primary" size="small" @click="loadTypes">刷新</el-button>
      </div>-->
      <div class="type-list">
        <template v-for="g in typeGroups" :key="g.name">
          <div class="type-group">{{ g.name }}</div>
          <div v-for="item in g.items" :key="item.name"
               class="type-item" :class="{ active: query.type === item.name }"
               @click="selectType(item.name)">
            <el-icon :size="15"><component :is="typeIcon(item.name)" /></el-icon>
            <span>{{ item.label }}</span>
            <span v-if="item.label !== item.name" class="type-key">{{ item.name }}</span>
          </div>
        </template>
      </div>
    </div>

    <!-- 右侧列表 -->
    <div class="nodes-list">
      <div style="display:flex;gap:8px;align-items:center;margin-bottom:14px;flex-wrap:wrap;">
        <span style="font-size:16px;">{{ query.type || '未选择类型' }}</span>
        <!-- 显示名检索：针对类型的 admin.display 字段（内核的 contains ⇒ LIKE '%…%'）。
             只在类型声明了 display 时才出现 —— 没有它就没法拼条件（宁可不出这个框, 也别静默搜不到）。 -->
        <el-input v-if="displayField" v-model="query.search" size="small" clearable
                  :placeholder="'搜索' + displayLabel" style="width:200px;"
                  @keyup.enter="applyFilters" @clear="applyFilters" @input="onSearchInput">
          <template #prefix><el-icon><Search /></el-icon></template>
        </el-input>
        <!-- 引用筛选：类型的每个 ref 字段一项, 与编辑表单同款的可搜索选择 -->
        <el-popover v-for="f in filters" :key="f.field" trigger="click" placement="bottom-start"
                    :show-timeout="0" :hide-timeout="0" :width="280" style="margin-left:8px;"
                    :ref="'fp-' + f.field" @show="onFilterOpen(f)">
          <template #reference>
            <el-button link size="small" class="filter-link" :class="{ active: f.active }">
              {{ f.active ? (f.activeLabel + ' ✕') : ('按' + f.label + '筛选') }}
            </el-button>
          </template>
          <el-select :model-value="f.active || null" filterable clearable
                     :loading="f.loading" placeholder="搜索并选择" style="width:100%"
                     @update:model-value="(v) => pickRef(f, v)">
            <el-option v-for="o in f.options" :key="o.id" :label="o.label" :value="o.id" />
          </el-select>
        </el-popover>
        <el-button size="small" @click="refresh"><el-icon><Refresh /></el-icon>刷新</el-button>
        <div style="flex:1;"></div> <!-- 右侧靠拢 -->
        <el-button type="primary" size="small" :disabled="!query.type" @click="createVisible = true">
          <el-icon><Plus /></el-icon>新建 {{ query.type ? typeLabel(query.type) : '' }}
        </el-button>
      </div>

      <!-- 树视图（admin.view: tree 的类型）: 服务端装好的整棵树, 不分页 -->
      <el-table v-if="treeMode" :data="treeNodes" v-loading="loading" row-key="id"
                :tree-props="{ children: 'children' }" default-expand-all>
        <el-table-column label="ID" width="80">
          <template #default="{ row: r }">
            <a class="node-title-link" @click.prevent="openEdit(r)">#{{ r.id }}</a>
          </template>
        </el-table-column>
        <el-table-column v-for="c in adminColumns" :key="c" :label="fieldLabel(c)" min-width="130" show-overflow-tooltip>
          <template #default="{ row: r }">
            <component v-if="cellOf(c)" :is="cellOf(c)" mode="cell" :model-value="fieldOf2(r, c)"
                       :field="fieldDef(c)" :defs="typeDefs" :node="r" @open-node="openRef" />
            <span v-else-if="isStruct(c)" class="cell-struct">{{ structSummary(r, c) }}</span>
            <span v-else class="cell-error">字段 {{ c }} 没有 kind</span>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="300" fixed="right">
          <template #default="{ row: r }">
            <node-ops :node="r" :defs="typeDefs" :type-name="query.type" show-create
                      :parent-id="r.id" @changed="refresh" />
          </template>
        </el-table-column>
      </el-table>

      <el-table v-else :data="rows" v-loading="loading">
        <!-- ID 是可点的编辑入口（v2 那列"标题"是 display 系统列的幽灵, 已删:
             显示什么完全由 types 的 admin.columns 决定） -->
        <el-table-column label="ID" width="80">
          <template #default="{ row: r }">
            <a class="node-title-link" @click.prevent="openEdit(r)">#{{ r.id }}</a>
          </template>
        </el-table-column>
        <el-table-column v-for="c in adminColumns" :key="c" :label="fieldLabel(c)" min-width="130" show-overflow-tooltip>
          <template #default="{ row: r }">
            <component v-if="cellOf(c)" :is="cellOf(c)" mode="cell" :model-value="fieldOf2(r, c)"
                       :field="fieldDef(c)" :defs="typeDefs" :node="r" @open-node="openRef" />
            <span v-else-if="isStruct(c)" class="cell-struct">{{ structSummary(r, c) }}</span>
            <span v-else class="cell-error">字段 {{ c }} 没有 kind</span>
          </template>
        </el-table-column>
        <el-table-column label="更新时间" width="165">
          <template #default="{ row: r }">{{ fmt(r.updated_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" width="200" fixed="right">
          <template #default="{ row: r }">
            <node-ops :node="r" :defs="typeDefs" :type-name="query.type" @changed="refresh" />
          </template>
        </el-table-column>
      </el-table>

      <div v-if="!treeMode" style="display:flex;justify-content:flex-end;margin-top:12px;">
        <el-pagination background layout="total, prev, pager, next" :total="total"
                       :page-size="query.size" :current-page="query.page"
                       @current-change="onPageChange" />
      </div>
      <!-- 页面级新建（行编辑走 NodeOps） -->
      <node-edit-dialog v-model:visible="createVisible" :type-name="query.type" :defs="typeDefs"
                        @changed="refresh" />
      <!-- 行内 ID 链接打开的编辑对话框 -->
      <node-edit-dialog v-model:visible="editVisible" :node="editNode" :type-name="query.type"
                        :is-edit="true" :defs="typeDefs" @changed="refresh" />
      <!-- 点引用链接：目标多半是别的类型，所以单独一个抽屉（类型跟着目标走） -->
      <node-edit-dialog v-model:visible="refEdit.visible" :node="refEdit.node" :type-name="refEdit.typeName"
                        :is-edit="true" :defs="typeDefs" @changed="refresh" />
    </div>




  </div>
</template>

<script>
import FieldRenderer from './FieldRenderer.vue'
export default {
    name: 'NodesPage',
    components: {
        FieldRenderer,
        NodeOps: Vue.defineAsyncComponent(() => window.Panel.loadComponent('pages/NodeOps.vue')),
        NodeEditDialog: Vue.defineAsyncComponent(() => window.Panel.loadComponent('pages/NodeEditDialog.vue')),
    },
    data() {
        return {
            typeNames: [],
            typeDefs: {},
            rows: [],
            total: 0,
            loading: false,
            treeMode: false,     // admin.view === 'tree' ⇒ 用树形表（全量不分页）
            treeNodes: [],
            filters: [],       // 引用筛选: [{field, to, label, options, active, activeLabel}]
            query: { type: '', filter: '', search: '', page: 1, size: 25 },
            searchTimer: null,   // 显示名检索的输入防抖
            createVisible: false,
            editVisible: false,
            refEdit: { visible: false, node: null, typeName: '' },   // 点引用链接打开的节点（类型可能不同）
            editNode: null,
        }
    },
    computed: {
        // 左侧类型列表：没填 admin.group 的排最前（不归入"其他"），其余按首次出现的顺序
        typeGroups() {
            return this.buildTypeGroups()
        },
        adminColumns() {
            const def = this.typeDefs[this.query.type] || {}
            return ((def.admin && def.admin.columns) || []).filter(c => !['id', 'updated_at'].includes(c))
        },
        // 检索框针对的字段 = 类型的展示字段（admin.display）; 没声明就不显示检索框
        displayField() {
            const def = this.typeDefs[this.query.type] || {}
            return (def.admin && def.admin.display) || ''
        },
        displayLabel() {
            const def = this.typeDefs[this.query.type] || {}
            const fields = def.fields || []
            const found = fields.filter(f => f.name === this.displayField)[0]
            return (found && found.label) || this.displayField || '名称'
        },
    },
    async mounted() { await this.loadTypes() },
    methods: {
        // 类型的中文名：admin.label 缺省回退类型名（配置键），没填的 types.yaml 照常工作
        typeLabel(name) {
            const admin = (this.typeDefs[name] || {}).admin || {}
            return admin.label || name
        },
        // 分组只作用于左侧类型列表：没填 group 的按名字并进"未分组"（站点自己叫这个名也并进来，
        // 不做特殊处理），这一节固定排最前；组内顺序一律按类型键（typeNames 已是键序），组按首次出现。
        buildTypeGroups() {
            const groups = []
            const byName = {}
            this.typeNames.forEach(name => {
                const admin = (this.typeDefs[name] || {}).admin || {}
                const group = admin.group || '未分组'
                if (!byName[group]) {
                    byName[group] = { name: group, items: [] }
                    groups.push(byName[group])
                }
                byName[group].items.push({ name, label: admin.label || name })
            })
            const at = groups.findIndex(g => g.name === '未分组')
            if (at > 0) groups.unshift(groups.splice(at, 1)[0])
            return groups
        },
        // ID 链接 → 编辑对话框
        openEdit(node) {
            this.editNode = node
            this.editVisible = true
        },
        // 图标来自 Admin View，不参与数据语义。
        typeIcon(t) {
            const def = this.typeDefs[t] || {}
            return (def.admin && def.admin.icon) || 'Files'
        },
        fieldLabel(name) {
            const def = this.typeDefs[this.query.type] || {}
            const field = (def.fields || []).find(f => f.name === name)
            return (field && field.label) || name
        },
        // 结构字段（array/object）没有组件文件: 列表里给个摘要, 不显示"缺组件"。
        isStruct(name) {
            const f = this.fieldDef(name)
            return !!f && (f.kind === 'array' || f.kind === 'object')
        },
        structSummary(row, name) {
            const v = this.fieldOf2(row, name)
            if (Array.isArray(v)) return v.length + ' 项'
            if (v && typeof v === 'object') return Object.keys(v).length + ' 个键'
            return '—'
        },
        // 单元格: ref/refs 的值不在 fields 里（存 edges 表），只能取 expand 里的显示名 ——
        // 列表接口本来就批量展开了一层出边，这里只是把它用上。
        // 列 → 字段定义 → 字段 kind 名对应的组件（web/admin/widgets/<kind>.vue）。
        fieldDef(name) {
            const def = this.typeDefs[this.query.type] || {}
            return (def.fields || []).find(f => f.name === name) || null
        },
        // 引用链接触发：只给 id+type（NodeEditDialog 会拉全量值 + 引用回显）
        openRef(target) {
            if (!target || !target.id) return
            this.refEdit = { visible: true, node: { id: target.id, type: target.type }, typeName: target.type }
        },
        cellOf(name) {
            const field = this.fieldDef(name)
            return Widgets.resolve(field && field.kind)
        },
        // 引用值不在 fields 里（存 edges），列表接口批量展开在 node.expand
        fieldOf2(node, name) { return node.fields ? node.fields[name] : undefined },
        async loadTypes() {
            const res = await window.$api.types()
            this.typeDefs = res.types || {}
            this.typeNames = Object.keys(this.typeDefs).sort()
            // 默认选第一个
            if (!this.query.type && this.typeNames.length) this.selectType(this.typeNames[0])
        },
        selectType(t) {
            this.query.type = t
            this.query.page = 1
            // 类型切换清残留: 筛选表达式是按旧类型的字段填的（对不上新类型, fail-loud 报错）
            this.query.filter = ''
            this.query.search = ''
            this.setupFilters(this.typeDefs[t] || {})
            const def = this.typeDefs[t] || {}
            this.treeMode = !!(def.admin && def.admin.view === 'tree')
            if (this.treeMode) this.loadTree()
            else this.refresh()
        },
        // 引用筛选：类型的每个 ref/refs 字段一项（自引用除外 —— 那种类型的列表本身就是树）。
        // 目标类型声明了 tree capability → 用分类树选（含子树，多字段 AND）；
        // 其他目标 → 与编辑表单同款的远程搜索选择（一个节点）。
        setupFilters(def) {
            this.filters = []
            if (!def) return
            const name = def.name
            for (const f of def.fields || []) {
                if (f.kind !== 'ref' && f.kind !== 'refs') continue
                if (f.to === name) continue
                this.filters.push({
                    field: f.name, to: f.to, label: f.label || f.name,
                    options: [], active: 0, activeLabel: '', loading: false, loaded: false,
                })
            }
        },
        // 打开时先给一批候选（不然得先打字才看得到选项）; 过滤由组件的 filterable 在本地做
        //（没有检索端点 —— 内核不带全文检索; 候选是分类/地区这类小集合）。
        onFilterOpen(f) {
            if (!f.loaded) this.loadFilterRef(f)
        },
        async loadFilterRef(f) {
            f.loading = true
            try {
                const res = await window.$api.nodes(f.to, { page: 1, size: 100, sort: '-id' })
                f.options = (res.items || []).map(n => ({ id: n.id, label: this.titleOf(n) + ' #' + n.id }))
                f.loaded = true
            } catch (_) {
                f.options = []
            } finally {
                f.loading = false
            }
        },
        // 选了一个节点（非树目标）; 清空时 id 为 undefined
        pickRef(f, id) {
            f.active = id || 0
            const hit = (f.options || []).find(o => o.id === id)
            f.activeLabel = hit ? hit.label : '#' + id
            f._ids = id ? [id] : null
            this.applyFilters()
        },
        // 显示名检索的输入防抖: 打字停 300ms 才查（Enter 与清空立即查）
        onSearchInput() {
            if (this.searchTimer) clearTimeout(this.searchTimer)
            this.searchTimer = setTimeout(this.applyFilters, 300)
        },
        clearFilter(ft) {
            ft.active = 0
            ft.activeLabel = ''
            ft._ids = null
            this.applyFilters()
        },
        applyFilters() {
            this.query.filter = this.combineFilters()
            this.query.page = 1
            this.refresh()
        },
        closeFilterPopover(ft) {
            const ref = this.$refs['fp-' + ft.field]
            if (ref && ref[0]) ref[0].hide()
        },
        // 多字段 AND 组合: 引用筛选 + 显示名检索
        //   (and (in ->category [1 2]) (contains $name "新能源"))
        // 字面量用 JSON.stringify: so 的解析走 strconv.Unquote（同一套转义）⇒ 引号/反斜杠安全;
        // 内核的 contains 自己参数化 + escapeLike ⇒ 用户输入的 % _ 不会变成通配。
        combineFilters() {
            const parts = []
            for (const f of this.filters) {
                if (f._ids && f._ids.length) {
                    parts.push('(in ->' + f.field + ' [' + f._ids.join(' ') + '])')
                }
            }
            const term = String(this.query.search || '').trim()
            if (term && this.displayField) {
                parts.push('(contains $' + this.displayField + ' ' + JSON.stringify(term) + ')')
            }
            if (parts.length === 0) return ''
            if (parts.length === 1) return parts[0]
            return '(and ' + parts.join(' ') + ')'
        },
        // 树模式: 一次装整棵（服务端不受分页上限影响; 读不到的行不进树）。
        // 排序交给服务端: admin.columns 里的第一个数值字段? 不猜 —— 用列表同一个 sort 约定
        //（默认内核序）, 需要自定义就在这儿给它一个 sort 串。
        async loadTree() {
            if (!this.query.type) return
            this.loading = true
            try {
                const res = await window.$api.tree(this.query.type, '')
                this.treeNodes = res.items || []
                this.total = res.total || 0
            } catch (_) {
                this.treeNodes = []
            } finally { this.loading = false }
        },
        async refresh() {
            if (!this.query.type) return
            if (this.treeMode) return this.loadTree()
            this.loading = true
            try {
                const params = { page: this.query.page, size: this.query.size, sort: '-id' }
                if (this.query.filter) params.filter = this.query.filter
                const res = await window.$api.nodes(this.query.type, params)
                this.rows = res.items || []
                this.total = res.total || 0
            } finally { this.loading = false }
        },
        onPageChange(p) { this.query.page = p; this.refresh() },
        // 时间列是 **Unix 秒**（整数）—— 显示按设备本地时间（v2 是 ISO 字符串,
        // 端口时这里漏了: `s.replace` 在数字上直接抛 TypeError）。
        fmt(s) { return s ? Widgets.localTime(s) : '' },
        // 引用筛选选项/回显的标签: 统一走 $api.refLabel
        // （admin.columns 首个非空 → 字段序 → expand 合成 → #id 兜底）
        titleOf(r) {
            return window.$api.refLabel(r, this.typeDefs[r.type] || null)
        },
    },
}</script>

<style>
.nodes-page { display: flex; flex: 1; min-height: 0; }
.nodes-tree {
    width: 250px; flex-shrink: 0;
    padding: 16px 16px 16px 0;
    overflow: auto;
    border-right: 1px solid #eaeaee; /* 中间竖线分隔 */
    font-family: 'Segoe UI', 'Segoe UI Web (West European)', -apple-system, 'system-ui', Roboto, 'Helvetica Neue', sans-serif;
}
.nodes-tree-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; font-size: 13px; color: #616161; }
.nodes-list {
    flex: 1; min-width: 0;
    padding: 16px 0 16px 16px;
}
.type-list { display: flex; flex-direction: column; gap: 0; }
/* 分组标题：文字后面接一条贯穿线（legend 的感觉），左对齐、不画方框 */
.type-group { display: flex; align-items: center; gap: 8px; margin: 14px 0 4px; font-size: 11px; color: #a19f9d; }
.type-group::after { content: ''; flex: 1; height: 1px; background: #edebe9; }
.type-group:first-child { margin-top: 2px; }
.type-item {
    display: flex; align-items: center; gap: 8px;
    padding: 7px 16px; border-radius: 0; cursor: pointer;
    font-size: 13px; color: #242424; border-left: 2px solid transparent;
}
.type-item:hover { background: #f3f2f1; }
.type-item.active { background: #edebe9; border-left: 2px solid #0277d4; color: #242424; font-weight: 600; }
.type-item.active:hover { background: #e1dfdd; }
.type-key { font-size: 11px; font-weight: 400; color: #a19f9d; }

/* 树过滤按钮: link 下划线样式（非按钮框 — 看着轻） */
.filter-link {
    color: #000000a6 !important;
    text-decoration: underline;
    &:hover { color: #000 !important; }
    &.active { color: #000 !important; font-weight: 600; }
}

/* el-tree 节点: 平时透明, hover 浅灰, current #edebe9 + 左 border */
.el-tree--highlight-current .el-tree-node.is-current > .el-tree-node__content {
    background: #edebe9 !important;
    color: #242424;
    border-left: 2px solid #0277d4;
    padding-left: 14px;
}
.el-tree-node__content:hover{ background:#f3f2f1; }
.el-tree-node__content{ color:#242424; font-size:13px; }
</style>
