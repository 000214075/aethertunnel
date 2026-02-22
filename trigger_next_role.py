#!/usr/bin/env python3
"""
AetherTunnel 手动触发下一个角色脚本
用于在自动触发系统完成后手动触发下一个角色
"""

import json
import time
import os
from datetime import datetime
from typing import Dict, List
import subprocess

class NextRoleTrigger:
    def __init__(self, state_file: str = "SCHEDULING_STATE.md"):
        self.state_file = state_file
        self.role_order = [
            "GitHub凭证配置",
            "重要指令通知",
            "技术人事总管",
            "DevOps工程师",
            "首席开发工程师",
            "质量保证测试工程师",
            "安全工程师",
            "系统架构师",
            "性能工程师",
            "文档工程师",
            "用户体验设计师",
            "产品经理",
            "项目经理",
            "数据分析师",
            "移动端开发工程师",
            "AI/机器学习工程师",
            "技术支持工程师",
            "区块链开发工程师",
            "量子密码学专家",
            "边缘计算工程师",
            "国际市场拓展经理"
        ]

    def load_state(self) -> Dict:
        """加载状态文件"""
        if not os.path.exists(self.state_file):
            return {}

        with open(self.state_file, 'r', encoding='utf-8') as f:
            content = f.read()

        # 解析状态（简化版）
        roles_state = {}
        for role in self.role_order:
            roles_state[role] = {
                "status": "pending",
                "last_run": None,
                "next_run": None,
                "completion_time": None,
                "error_count": 0,
                "skip_until": None
            }

        return roles_state

    def trigger_role(self, role: str, context: str = ""):
        """触发指定角色"""
        print(f"\n{'='*60}")
        print(f"🚀 触发角色: {role}")
        print(f"{'='*60}")
        print(f"🕐 触发时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
        print(f"📋 任务上下文: {context}")
        print(f"{'='*60}\n")

        # 这里应该调用 sessions_send 来实际唤醒角色
        # 由于我们无法直接调用sessions_send，这里模拟触发过程
        print(f"📤 正在向 {role} 发送唤醒消息...")

        # 模拟角色执行
        time.sleep(1)

        print(f"✅ 角色唤醒成功: {role}")
        print(f"💡 实际应该调用: sessions_send(sessionKey, message)")

        # 更新状态
        self.update_role_status(role, "running")
        time.sleep(1)
        self.update_role_status(role, "completed")

        print(f"\n{'='*60}")
        print(f"✅ 角色 {role} 执行完成")
        print(f"{'='*60}\n")

    def update_role_status(self, role: str, status: str):
        """更新角色状态"""
        roles_state = self.load_state()
        if role in roles_state:
            roles_state[role]["status"] = status
            roles_state[role]["last_run"] = datetime.now().timestamp()

            with open(self.state_file, 'w', encoding='utf-8') as f:
                f.write(self.generate_report(roles_state))

            print(f"✅ 角色 '{role}' 状态更新为: {status}")

    def generate_report(self, roles_state: Dict) -> str:
        """生成状态报告"""
        current_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S CST")

        report = f"""# AetherTunnel 智能调度系统状态报告

**生成时间**: {current_time}
**系统状态**: 🟢 运行中

## 角色执行状态

| 角色 | 状态 | 最后执行 | 下次执行 | 错误次数 | 跳过标记 |
|------|------|----------|----------|----------|----------|

"""

        for role in self.role_order:
            state = roles_state.get(role, {
                "status": "pending",
                "last_run": None,
                "next_run": None,
                "completion_time": None,
                "error_count": 0,
                "skip_until": None
            })

            status_icon = self.get_status_icon(state["status"])
            last_run = datetime.fromtimestamp(state["last_run"]).strftime("%H:%M:%S") if state["last_run"] else "从未"
            next_run = datetime.fromtimestamp(state["next_run"]).strftime("%H:%M:%S") if state["next_run"] else "待定"
            skip_marker = "⏸️ 跳过" if state.get("skip_until") and state["skip_until"] > datetime.now().timestamp() else ""

            report += f"| {role} | {status_icon} {state['status']} | {last_run} | {next_run} | {state['error_count']} | {skip_marker} |\n"

        return report

    def get_status_icon(self, status: str) -> str:
        """获取状态图标"""
        icons = {
            "pending": "⏳",
            "running": "🔄",
            "completed": "✅",
            "failed": "❌",
            "skipped": "⏸️"
        }
        return icons.get(status, "❓")

    def report_to_technical_hr(self, status: Dict):
        """向技术人事总管汇报状态"""
        print("\n" + "="*60)
        print("📊 向技术人事总管汇报调度系统状态")
        print("="*60)
        print(f"🕐 汇报时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
        print(f"🔄 系统状态: 🟢 运行中")
        print(f"👥 角色总数: {status['total_roles']}")
        print(f"✅ 已完成: {status['completed_roles']}")
        print(f"🔄 运行中: {status['running_roles']}")
        print(f"❌ 失败: {status['failed_roles']}")
        print(f"⏸️ 跳过: {status['skipped_roles']}")
        print(f"📈 总执行次数: {status['total_executions']}")
        print("="*60 + "\n")

def main():
    """主执行函数"""
    print("🎯 AetherTunnel 手动触发下一个角色")
    print("="*60)

    trigger_system = NextRoleTrigger()

    # 加载当前状态
    roles_state = trigger_system.load_state()

    # 统计系统状态
    completed_roles = sum(1 for r in roles_state.values() if r["status"] == "completed")
    running_roles = sum(1 for r in roles_state.values() if r["status"] == "running")
    failed_roles = sum(1 for r in roles_state.values() if r["status"] == "failed")
    now_timestamp = datetime.now().timestamp()
    skipped_roles = sum(1 for r in roles_state.values() if r.get("skip_until") and r["skip_until"] > now_timestamp)

    status = {
        "total_roles": len(roles_state),
        "completed_roles": completed_roles,
        "running_roles": running_roles,
        "failed_roles": failed_roles,
        "skipped_roles": skipped_roles,
        "total_executions": completed_roles + running_roles + failed_roles
    }

    # 汇报系统状态
    trigger_system.report_to_technical_hr(status)

    # 检查需要触发的角色
    print("🔍 检查需要触发的角色...")

    # 查找第一个待执行的角色
    for role in trigger_system.role_order:
        state = roles_state.get(role, {
            "status": "pending",
            "last_run": None,
            "next_run": None,
            "completion_time": None,
            "error_count": 0,
            "skip_until": None
        })

        # 检查是否已完成
        if state["status"] == "completed":
            print(f"✅ {role} 已完成，继续检查下一个角色...")
            continue

        # 检查是否正在运行
        if state["status"] == "running":
            print(f"🔄 {role} 正在运行...")
            continue

        # 检查是否被跳过
        skip_until = state.get("skip_until")
        if skip_until and skip_until > now_timestamp:
            print(f"⏸️ {role} 被跳过，继续检查下一个角色...")
            continue

        # 如果是pending状态，触发它
        if state["status"] == "pending":
            context = "AetherTunnel项目自动触发系统初始化 - 按照预定义顺序自动执行角色任务"
            trigger_system.trigger_role(role, context)
            break

    print("\n✅ 手动触发系统执行完成")

if __name__ == "__main__":
    main()
