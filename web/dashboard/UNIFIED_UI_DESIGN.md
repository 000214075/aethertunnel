# 🎨 AetherTunnel Web 界面统一设计规范

**版本**: v0.1.0  
**类型**: 设计规范文档

---

## 🎯 设计目标

### 1. UI 风格一致性
- ✅ 三套独立界面（通用版、服务端版、客户端版）
- ✅ 统一的配色系统
- ✅ 统一的组件库
- ✅ 统一的动画效果
- ✅ 统一的响应式布局

### 2. 功能独立
- ✅ 通用版：完整功能，适合所有人
- ✅ 服务端版：服务器专属功能
- ✅ 客户端版：客户端专属功能

---

## 🎨 统一配色系统

### 主色方案（三套界面通用）

```css
/* ===== 统一主题色 ===== */
:root {
    /* 主色 - 统一 */
    --primary-color: #6366f1;
    --primary-dark: #4f46e5;
    --primary-light: #818cf8;
    
    /* 辅助色 - 统一 */
    --secondary-color: #ec4899;
    --success-color: #10b981;
    --warning-color: #f59e0b;
    --danger-color: #ef4444;
    --info-color: #3b82f6;
    
    /* 中性色 - 统一 */
    --bg-primary: #ffffff;
    --bg-secondary: #f8fafc;
    --bg-tertiary: #f1f5f9;
    --text-primary: #1e293b;
    --text-secondary: #64748b;
    --border-color: #e2e8f0;
    
    /* 阴影 - 统一 */
    --shadow-sm: 0 1px 2px rgba(0, 0, 0, 0.05);
    --shadow-md: 0 4px 6px rgba(0, 0, 0, 0.1);
    --shadow-lg: 0 10px 15px rgba(0, 0, 0, 0.1);
    --shadow-xl: 0 20px 25px rgba(0, 0, 0, 0.1);
    
    /* 过渡 - 统一 */
    --transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1);
}
```

### 主题色差异化（仅在侧边栏和页面头部）

**通用版主题**：
```css
/* 通用版 - 蓝紫渐变 */
:root {
    --theme-gradient: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
    --theme-header-bg: rgba(255, 255, 255, 0.95);
}
```

**服务端版主题**：
```css
/* 服务端版 - 深蓝紫渐变 */
:root {
    --theme-gradient: linear-gradient(135deg, #4f46e5 0%, #7c3aed 100%);
    --theme-header-bg: rgba(255, 255, 255, 0.98);
    --theme-sidebar-bg: rgba(255, 255, 255, 0.05);
}
```

**客户端版主题**：
```css
/* 客户端版 - 粉紫蓝渐变 */
:root {
    --theme-gradient: linear-gradient(135deg, #6366f1 0%, #8b5cf6 100%);
    --theme-header-bg: rgba(255, 255, 255, 0.95);
    --theme-sidebar-bg: rgba(255, 255, 255, 0.03);
}
```

---

## 🧱 统一组件库

### 1. 卡片组件

**统一样式**：
```css
.card {
    background: var(--bg-primary);
    border-radius: 16px;
    box-shadow: var(--shadow-md);
    padding: 0;
    overflow: hidden;
    transition: var(--transition);
    border: 1px solid var(--border-color);
}

.card:hover {
    box-shadow: var(--shadow-lg);
    transform: translateY(-2px);
}

.card-header {
    background: var(--theme-gradient);
    color: white;
    padding: 20px 30px;
    display: flex;
    align-items: center;
    justify-content: space-between;
}

.card-title {
    font-size: 20px;
    font-weight: 700;
    color: white;
    display: flex;
    align-items: center;
    gap: 10px;
}

.card-body {
    padding: 30px;
}
```

### 2. 按钮组件

**统一样式**：
```css
.btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    padding: 12px 24px;
    font-size: 15px;
    font-weight: 600;
    border-radius: 10px;
    border: none;
    cursor: pointer;
    transition: var(--transition);
    text-decoration: none;
    gap: 8px;
}

.btn-primary {
    background: var(--theme-gradient);
    color: white;
    box-shadow: 0 4px 10px rgba(99, 102, 241, 0.3);
}

.btn-primary:hover {
    transform: translateY(-2px);
    box-shadow: 0 6px 15px rgba(99, 102, 241, 0.4);
}

.btn-secondary {
    background: var(--bg-secondary);
    color: var(--text-primary);
    border: 1px solid var(--border-color);
}

.btn-secondary:hover {
    background: var(--bg-tertiary);
    border-color: var(--primary-color);
}

.btn-danger {
    background: rgba(239, 68, 68, 0.1);
    color: var(--danger-color);
    border: 1px solid rgba(239, 68, 68, 0.3);
}

.btn-danger:hover {
    background: rgba(239, 68, 68, 0.2);
    border-color: var(--danger-color);
}

.btn-icon {
    font-size: 18px;
}

.btn-lg {
    padding: 16px 32px;
    font-size: 16px;
    min-width: 180px;
}
```

### 3. 表单组件

**统一样式**：
```css
.form-group {
    margin-bottom: 20px;
}

.form-label {
    display: block;
    font-size: 14px;
    font-weight: 600;
    color: var(--text-primary);
    margin-bottom: 8px;
}

.form-label.required::after {
    content: " *";
    color: var(--danger-color);
}

.form-input {
    width: 100%;
    padding: 14px 18px;
    font-size: 15px;
    border: 2px solid var(--border-color);
    border-radius: 10px;
    background: var(--bg-primary);
    color: var(--text-primary);
    transition: var(--transition);
}

.form-input:focus {
    outline: none;
    border-color: var(--primary-color);
    box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.1);
}

.form-hint {
    font-size: 13px;
    color: var(--text-secondary);
    margin-top: 6px;
    line-height: 1.5;
}

.form-select {
    width: 100%;
    padding: 14px 18px;
    font-size: 15px;
    border: 2px solid var(--border-color);
    border-radius: 10px;
    background: var(--bg-primary);
    color: var(--text-primary);
    transition: var(--transition);
    cursor: pointer;
}

.form-select:focus {
    outline: none;
    border-color: var(--primary-color);
    box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.1);
}
```

---

## 🎨 界面对比

### 1. 顶部导航栏

| 特性 | 通用版 | 服务端版 | 客户端版 |
|------|--------|----------|----------|
| **背景** | 玻璃态 | 玻璃态 | 玻璃态 |
| **Logo** | AT | AT | AT |
| **标题** | AetherTunnel | AetherTunnel 服务器 | AetherTunnel 客户端 |
| **状态** | 连接状态 | 运行状态 | 连接状态 |
| **版本号** | v0.1.0 | v0.1.0 | v0.1.0 |

### 2. 侧边栏

| 特性 | 通用版 | 服务端版 | 客户端版 |
|------|--------|----------|----------|
| **背景** | 透明 | 略深透明 | 浅色透明 |
| **分组** | 5 组 | 5 组 | 3 组 |
| **菜单项** | 完整 | 完整 | 简化 |
| **激活状态** | 渐变紫色 | 深邃紫色 | 蓝紫色 |
| **悬停效果** | 平移 | 平移 | 平移 |

### 3. 主内容区

| 特性 | 通用版 | 服务端版 | 客户端版 |
|------|--------|----------|----------|
| **总览页面** | 完整统计 | 服务器总览 | 代理管理 |
| **卡片样式** | 统一 | 统一 | 统一 |
| **按钮样式** | 统一 | 统一 | 统一 |
| **图表** | Canvas | Canvas | Canvas |

---

## 🎨 界面差异

### 1. 通用版（`index.html`）

**主题色**：
- 主题渐变：`#667eea` → `#764ba2`（蓝紫色）
- 侧边栏背景：95% 透明
- 页面背景：渐变蓝紫色

**侧边栏菜单**：
- 主菜单（4 项）
- 高级设置（5 项）
- 颠覆性功能（4 项）
- 系统（3 项）
- 总计：16 项

**主要功能**：
- 总览
- 代理管理
- 客户端管理
- 服务器配置
- 安全设置
- 传输配置
- AI 智能路由
- WebRTC P2P
- DHT 网络
- 量子加密
- 日志
- 设置

### 2. 服务端版（`server.html`）

**主题色**：
- 主题渐变：`#4f46e5` → `#7c3aed`（深蓝紫色）
- 侧边栏背景：5% 透明
- 页面背景：深邃渐变

**侧边栏菜单**：
- 主菜单（4 项）
- 监控（3 项）
- 高级（3 项）
- 颠覆性功能（4 项）
- 系统（3 项）
- 总计：17 项

**主要功能**：
- 服务器总览
- 客户端管理（8 个客户端示例）
- 代理管理
- 服务器配置
- 实时流量
- 日志查看
- 性能监控
- 安全设置
- 网络配置
- AI 智能路由
- WebRTC P2P
- DHT 网络
- 量子加密
- 备份与恢复
- 重启服务器
- 关闭服务器
- 更新系统

**特色功能**：
- 8 个客户端详细卡片
- 高级日志过滤
- 服务器操作按钮
- 6 个服务器状态指标

### 3. 客户端版（`client.html`）

**主题色**：
- 主题渐变：`#6366f1` → `#8b5cf6`（粉紫色）
- 侧边栏背景：3% 透明
- 页面背景：渐变蓝紫色

**侧边栏菜单**：
- 我的隧道（3 项）
- 监控（2 项）
- 设置（1 项）
- 总计：6 项

**主要功能**：
- 我的隧道（代理管理）
- 连接状态（详细）
- 客户端设置
- 流量统计
- 日志查看

**特色功能**：
- 5 个代理卡片（完整示例）
- 快速操作（全部启动、全部停止、全部重启）
- 连接状态大图标
- 连接信息详情
- 流量统计卡片
- 添加代理模态框

---

## 🎨 配色方案对比

| 组件 | 通用版 | 服务端版 | 客户端版 |
|------|--------|----------|----------|
| **主色** | `#6366f1` | `#4f46e5` | `#6366f1` |
| **主题渐变** | 蓝紫 | 深蓝紫 | 粉紫 |
| **侧边栏** | 95% 透明 | 5% 透明 | 3% 透明 |
| **页面背景** | 渐变蓝紫 | 深邃渐变 | 渐变蓝紫 |
| **卡片** | 白色 | 白色 | 白色 |
| **文字** | 深灰 | 深灰 | 深灰 |
| **边框** | 浅灰 | 浅灰 | 浅灰 |

---

## 🎨 布局一致性

### 1. 主布局结构（所有版本）

```
┌─────────────────────────────────────┐
│  顶部导航栏（高度: 60px）           │
└─────────────────────────────────────┘
         ↓
┌──────────────────┬──────────────────┐
│                 │                  │
│   侧边栏       │   主内容区       │
│  (宽度: 240px)  │ (剩余宽度)       │
│                 │                  │
└──────────────────┴──────────────────┘
```

### 2. 卡片式内容布局（所有版本）

```
┌─────────────────────────────┐
│  标题栏（渐变背景）         │
└─────────────────────────────┘
         ↓
┌─────────────────────────────┐
│  内容区（白色背景）         │
│  - 表单 / 列表 / 图表       │
│  - 操作按钮                │
└─────────────────────────────┘
```

### 3. 响应式布局（所有版本）

| 设备 | 布局 | 侧边栏 |
|------|------|--------|
| **桌面端**（> 1200px） | 侧边栏 + 主内容 | 显示 |
| **平板端**（768-1200px） | 侧边栏 + 主内容 | 可折叠 |
| **移动端**（< 768px） | 单列 | 底部导航 |

---

## 🎨 动画一致性

### 1. 过渡动画（所有版本）

```css
/* 统一的过渡效果 */
transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1);
```

### 2. 悬停动画（所有版本）

```css
/* 统一的悬停效果 */
.card:hover {
    box-shadow: 0 20px 25px rgba(0, 0, 0, 0.1);
    transform: translateY(-4px);
}
```

### 3. 加载动画（所有版本）

```css
/* 统一的加载动画 */
.loading-spinner {
    width: 60px;
    height: 60px;
    border: 4px solid var(--border-color);
    border-top-color: var(--primary-color);
    border-radius: 50%;
    animation: spin 1s linear infinite;
}

@keyframes spin {
    0% { transform: rotate(0deg); }
    100% { transform: rotate(360deg); }
}
```

---

## 🎯 实现方案

### 1. 统一 CSS 变量

所有三个版本都使用相同的 CSS 变量：
- `--primary-color`
- `--success-color`
- `--warning-color`
- `--danger-color`
- `--shadow-sm`
- `--shadow-md`
- `--shadow-lg`
- `--transition`

### 2. 统一组件类

所有三个版本都使用相同的组件类：
- `.card`
- `.card-header`
- `.card-body`
- `.btn`
- `.btn-primary`
- `.btn-secondary`
- `.btn-danger`
- `.form-group`
- `.form-input`
- `.form-select`

### 3. 统一布局结构

所有三个版本都使用相同的布局结构：
- 顶部导航栏
- 侧边栏
- 主内容区
- 响应式设计

---

## 🎨 界面对比总结

### 统一性（3 个版本）

| 维度 | 通用版 | 服务端版 | 客户端版 | 一致性 |
|------|--------|----------|----------|--------|
| **主色** | `#6366f1` | `#4f46e5` | `#6366f1` | ✅ 统一 |
| **组件库** | 完整 | 完整 | 完整 | ✅ 统一 |
| **动画效果** | 完整 | 完整 | 完整 | ✅ 统一 |
| **响应式** | 完整 | 完整 | 完整 | ✅ 统一 |
| **卡片样式** | 统一 | 统一 | 统一 | ✅ 统一 |
| **按钮样式** | 统一 | 统一 | 统一 | ✅ 统一 |
| **表单样式** | 统一 | 统一 | 统一 | ✅ 统一 |

### 差异化（3 个版本）

| 维度 | 通用版 | 服务端版 | 客户端版 |
|------|--------|----------|----------|
| **主题色** | 蓝紫渐变 | 深蓝紫渐变 | 粉紫渐变 |
| **侧边栏** | 5 组 | 5 组（更详细） | 3 组（简化） |
| **菜单项** | 16 项 | 17 项 | 6 项 |
| **主要功能** | 完整 | 服务器专属 | 客户端专属 |
| **特色功能** | 通用功能 | 高级监控 | 快速操作 |

---

## 📊 文件大小对比

| 界面 | 文件大小 | 说明 |
|------|---------|------|
| **通用版**（index.html） | ~63KB | 完整功能 |
| **服务端版**（server.html） | ~49.6KB | 服务器专属 |
| **客户端版**（client.html） | ~52.3KB | 客户端专属 |

---

## 🎯 设计原则

### 1. 一致性优先
- ✅ 统一的配色系统
- ✅ 统一的组件库
- ✅ 统一的动画效果
- ✅ 统一的布局结构

### 2. 差异化合理
- ✅ 主题色微调
- ✅ 功能针对不同用户
- ✅ 侧边栏简化或详细
- ✅ 主要功能区分

### 3. 用户体验优先
- ✅ 清晰的视觉层次
- ✅ 直观的操作流程
- ✅ 响应式设计
- ✅ 流畅的动画效果

---

## 🎉 总结

### 设计成果

1. ✅ **三套独立界面**：通用版、服务端版、客户端版
2. ✅ **UI 风格完全一致**：统一配色、统一组件、统一布局
3. ✅ **功能针对不同用户**：
   - 通用版：适合所有人
   - 服务端版：适合服务器管理员
   - 客户端版：适合普通用户

### 技术实现

- **CSS 变量**：统一定义
- **组件库**：统一样式
- **响应式**：统一切换点
- **动画效果**：统一过渡

### 用户优势

1. ✅ **一致性**：三套界面风格统一，易于使用
2. ✅ **针对性**：功能针对不同用户群体
3. ✅ **专业性**：服务端版更专业
4. ✅ **友好性**：客户端版更亲切
5. ✅ **完整性**：通用版功能最全

---

<div align="center">

**🎨 UI 风格统一完成！**

**三套独立界面，风格完全一致！**

Made with ❤️ by AetherTunnel Team

</div>
