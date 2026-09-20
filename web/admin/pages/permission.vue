<template>
    <div>
        <h3 style="margin-top:0;">权限矩阵</h3>
        <p class="perm-note">
            值来自<b>规则求值</b>（不是照代码猜）：每种身份把读/写规则跑一遍，这里看到的就是运行时真正生效的。
            <b>读</b> = 看得见该字段；<b>建</b> = 新建该类型时可写该字段；<b>改</b> = 改该类型的
            <b>一个真实样本节点</b>时可写该字段（归属相关的规则只对样本成立，样本 id 在身份框的提示里）；
            <b>删</b> 是节点级权限，没有字段维度；<b>行范围</b>是读规则收窄出来的行集合
            （全 / 部分 / 无）—— 与字段可见性是两件事，写错时往往就是它。
        </p>
        <div class="perm-filter">
            <span class="perm-filter-label">身份</span>
            <el-checkbox v-model="withAnonymous">匿名</el-checkbox>
            <el-checkbox v-for="t in authTypes" :key="t.name" :label="t.name"
                :checked="selTypes.indexOf(t.name) >= 0"
                @change="toggleType(t.name)">{{ t.label }}</el-checkbox>
        </div>
        <div class="perm-filter">
            <span class="perm-filter-label">角色</span>
            <el-checkbox v-for="r in roleOptions" :key="r" :label="r"
                :checked="selRoles.indexOf(r) >= 0"
                @change="toggleRole(r)">{{ r }}</el-checkbox>
            <span class="perm-hint">角色加在**每个已勾身份**上（自由组合, 想对比就多勾一个类型）</span>
        </div>
        <el-tabs v-model="activeType">
            <el-tab-pane v-for="t in types" :key="t.type" :label="t.label || t.type" :name="t.type">
                <div class="perm-del">
                    <span class="perm-filter-label">行范围</span>
                    <span v-for="s in shownScenes" :key="s.id" class="perm-del-item">
                        {{ s.label }}<i class="perm-chip" :class="{ on: scopeOf(t, s.id) === 'all' }"
                            :title="scopeTitle(t, s.id)">{{ scopeLabel(t, s.id) }}</i>
                    </span>
                    <span class="perm-hint">读规则收窄出来的行集合：对应读接口能读到哪些节点（与字段可见性是两件事）。</span>
                </div>
                <div class="perm-del">
                    <span class="perm-filter-label">删除</span>
                    <span v-for="s in shownScenes" :key="s.id" class="perm-del-item">
                        {{ s.label }}<i class="perm-chip" :class="{ on: !!t.delete[s.id] }">删</i>
                    </span>
                    <span class="perm-hint">被引用不许删属于数据层限制（引擎 restrict），与权限无关。</span>
                </div>
                <el-table :data="rowsOf(t.type)" size="small" border height="calc(100vh - 430px)">
                    <el-table-column label="字段" width="220">
                        <template #default="sc">
                            {{ sc.row.field_label || sc.row.field }}
                        </template>
                    </el-table-column>
                    <el-table-column v-for="s in shownScenes" :key="s.id" :label="s.label" width="112" align="center">
                        <template #header>
                            <span :title="s.tip">{{ s.label }}</span>
                        </template>
                        <template #default="sc">
                            <i class="perm-chip" :class="{ on: sc.row.read[s.id] }" title="读">读</i>
                            <i class="perm-chip" :class="{ on: sc.row.create[s.id] }" title="新建时可写">建</i>
                            <i class="perm-chip" :class="{ on: sc.row.update[s.id] }" title="改时可写">改</i>
                        </template>
                    </el-table-column>
                </el-table>
            </el-tab-pane>
        </el-tabs>
    </div>
</template>
<script>
import $api from '$api'

export default {
    data() {
        return {
            authTypes: [],     // 能登录的类型（来自 /admin/types 的 capabilities.authentication）
            roleOptions: [],   // 可勾的角色（owner/admin + 各类型词表）
            selTypes: [],      // 勾了的身份类型（每个 = 表格的一列）
            selRoles: [],      // 勾了的角色（加在每个已勾身份上）
            withAnonymous: true,
            scenes: [],        // 身份档：匿名 + 每个 auth 类型 ×（无角色 / owner / admin / 词表角色）
            types: [],         // 类型级信息（标签 + 删除权限 + 样本 id）
            rows: [],          // 类型 × 字段 ×（读/建/改 三列）
            activeType: '',    // 当前 Tab
        }
    },
    computed: {
        shownScenes() { return this.scenes },
    },
    methods: {
        scopeOf(t, id) { return ((t.scope || {})[id]) || 'none' },
        scopeLabel(t, id) {
            return { all: '全', restricted: '部分', none: '无' }[this.scopeOf(t, id)] || '?'
        },
        scopeTitle(t, id) {
            const kind = this.scopeOf(t, id)
            if (kind === 'all') return '全部行都能读到（规则给的是恒真）'
            if (kind === 'restricted') return '只能读到满足条件的行'
            return '一行都读不到（规则给的是恒假, 或者这个类型没有读规则）'
        },
        rowsOf(type) {
            return this.rows.filter(function (r) { return r.type === type })
        },
        toggleType(name) {
            var i = this.selTypes.indexOf(name)
            if (i >= 0) this.selTypes.splice(i, 1)
            else this.selTypes.push(name)
            this.reload()
        },
        toggleRole(role) {
            var i = this.selRoles.indexOf(role)
            if (i >= 0) this.selRoles.splice(i, 1)
            else this.selRoles.push(role)
            this.reload()
        },
        // 勾了什么就求什么: 匿名 + 每个已勾类型（各带上勾选的角色集合）—— 服务端不预置组合
        actors() {
            var out = []
            if (this.withAnonymous) out.push('anonymous')
            var roles = this.selRoles.join(',')
            for (var i = 0; i < this.selTypes.length; i++) {
                out.push(roles ? this.selTypes[i] + ':' + roles : this.selTypes[i])
            }
            return out
        },
        async reload() {
            var actors = this.actors()
            if (!actors.length) { this.scenes = []; this.rows = []; return }
            var url = '/admin/permissions?' + actors.map(function (a) {
                return 'actor=' + encodeURIComponent(a)
            }).join('&')
            try {
                var res = await $api.get(url)
                this.apply(res)
            } catch (e) { console.error('[permission] reload failed:', e) }
        },
        apply(res) {
            this.scenes = (res.scenes || []).map(function (s) {
                s.tip = s.kind === 'anonymous' ? '未登录'
                    : (s.sample_id ? '样本节点 #' + s.sample_id + '（' + s.node_type + '）' : '')
                return s
            })
            this.types = res.types || []
            this.rows = res.rows || []
            if (!this.activeType && this.types.length) this.activeType = this.types[0].type
        },
        // 菜单（哪些能登录的类型 / 哪些角色）来自 /admin/types —— 不写死
        async loadMenu() {
            var res = await $api.types()
            var defs = res.types || {}
            var types = []
            var roles = { owner: true, admin: true }
            Object.keys(defs).sort().forEach(function (name) {
                var cap = (defs[name].capabilities || {}).authentication
                if (!cap) return
                types.push({
                    name: name,
                    label: (defs[name].admin && defs[name].admin.label) || name,
                    roles: cap.roles || [],
                })
                ;(cap.roles || []).forEach(function (r) { roles[r] = true })
            })
            this.authTypes = types
            this.roleOptions = Object.keys(roles).sort()
            this.selTypes = types.map(function (t) { return t.name })   // 默认全勾上
            this.withAnonymous = true
            await this.reload()
        },
    },
    mounted() { this.loadMenu().catch(function (e) { console.error('[permission] menu failed:', e) }) },
}
</script>
<style>
.perm-note { color: #6b7280; font-size: 12px; line-height: 1.8; margin: 0 0 10px; }
.perm-filter { display: flex; align-items: flex-start; gap: 10px; margin-bottom: 6px; flex-wrap: wrap; }
.perm-filter-label { color: #6b7280; font-size: 12px; line-height: 24px; }
.perm-del { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; margin: 0 0 8px; }
.perm-del-item { font-size: 12px; color: #374151; }
.perm-hint { color: #9ca3af; font-size: 12px; }
.perm-chip { display: inline-block; width: 22px; height: 20px; line-height: 20px; margin: 0 1px; font-style: normal;
    font-size: 11px; text-align: center; border-radius: 3px; background: #f3f4f6; color: #ffffff; }
.perm-chip.on { background: #16a34a; }
.perm-imm { font-style: normal; font-size: 10px; color: #b45309; background: #fef3c7; border-radius: 3px; padding: 0 3px; margin-left: 4px; }
</style>
