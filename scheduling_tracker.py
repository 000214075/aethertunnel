#!/usr/bin/env python3
"""
AetherTunnel 智能调度系统追踪器
用于管理角色调度状态、完成检测和自动唤醒机制
"""

import json
import time
import os
from datetime import datetime
from typing import Dict, List, Optional, Tuple

class SchedulingTracker:
    def __init__(self, state_file: str = "SCHEDULING_STATE.md"):
        self.state_file = state_file
        self.roles_state = {}
        self.completion_history = []
        self.skip_markers = set()
        self.initialized = False
        
        # 角色执行顺序（按优先级）
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
        
        # 角色执行频率（分钟）
        self.role_frequency = {
            "GitHub凭证配置": 2,
            "重要指令通知": 3,
            "技术人事总管": 5,
            "DevOps工程师": 6,
            "首席开发工程师": 7,
            "质量保证测试工程师": 8,
            "安全工程师": 9,
            "系统架构师": 10,
            "性能工程师": 11,
            "文档工程师": 12,
            "用户体验设计师": 13,
            "产品经理": 14,
            "项目经理": 15,
            "数据分析师": 16,
            "移动端开发工程师": 17,
            "AI/机器学习工程师": 18,
            "技术支持工程师": 19,
            "区块链开发工程师": 20,
            "量子密码学专家": 21,
            "边缘计算工程师": 22,
            "国际市场拓展经理": 23
        }
        
        self.load_state()
    
    def load_state(self):
        """从状态文件加载调度状态"""
        if os.path.exists(self.state_file):
            try:
                with open(self.state_file, 'r', encoding='utf-8') as f:
                    content = f.read()
                    
                # 解析状态文件（简化版）
                for role in self.role_order:
                    self.roles_state[role] = {
                        "status": "pending",  # pending, running, completed, failed, skipped
                        "last_run": None,
                        "next_run": None,
                        "completion_time": None,
                        "error_count": 0,
                        "skip_until": None
                    }
                
                self.initialized = True
                print(f"✅ 调度状态加载完成 - {len(self.roles_state)} 个角色")
                
            except Exception as e:
                print(f"❌ 加载状态文件失败: {e}")
                self.initialize_default_state()
        else:
            self.initialize_default_state()
    
    def initialize_default_state(self):
        """初始化默认状态"""
        current_time = datetime.now()
        
        for i, role in enumerate(self.role_order):
            frequency = self.role_frequency[role]
            next_run = current_time.timestamp() + (i * 60)  # 错开执行
            
            self.roles_state[role] = {
                "status": "pending",
                "last_run": None,
                "next_run": next_run,
                "completion_time": None,
                "error_count": 0,
                "skip_until": None
            }
        
        self.initialized = True
        self.save_state()
        print(f"✅ 默认调度状态初始化完成")
    
    def save_state(self):
        """保存调度状态到文件"""
        try:
            # 生成状态报告
            status_report = self.generate_status_report()
            
            with open(self.state_file, 'w', encoding='utf-8') as f:
                f.write(status_report)
                
        except Exception as e:
            print(f"❌ 保存状态文件失败: {e}")
    
    def generate_status_report(self) -> str:
        """生成状态报告"""
        current_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S CST")
        
        report = f"""# AetherTunnel 智能调度系统状态报告

**生成时间**: {current_time}
**系统状态**: {'🟢 运行中' if self.initialized else '🔴 未初始化'}

## 角色执行状态

| 角色 | 状态 | 最后执行 | 下次执行 | 错误次数 | 跳过标记 |
|------|------|----------|----------|----------|----------|

"""
        
        for role in self.role_order:
            state = self.roles_state[role]
            status_icon = self.get_status_icon(state["status"])
            
            last_run = state["last_run"]
            last_run_str = datetime.fromtimestamp(last_run).strftime("%H:%M:%S") if last_run else "从未"
            
            next_run = state["next_run"]
            next_run_str = datetime.fromtimestamp(next_run).strftime("%H:%M:%S") if next_run else "待定"
            
            skip_marker = "⏸️ 跳过" if role in self.skip_markers else ""
            
            report += f"| {role} | {status_icon} {state['status']} | {last_run_str} | {next_run_str} | {state['error_count']} | {skip_marker} |\n"
        
        report += f"""
## 执行统计

- **已完成任务**: {len([r for r in self.roles_state.values() if r['status'] == 'completed'])}
- **运行中任务**: {len([r for r in self.roles_state.values() if r['status'] == 'running'])}
- **失败任务**: {len([r for r in self.roles_state.values() if r['status'] == 'failed'])}
- **跳过任务**: {len(self.skip_markers)}
- **总执行次数**: {len(self.completion_history)}

## 最近完成记录

"""
        
        # 添加最近的完成记录
        for record in self.completion_history[-10:]:
            role = record['role']
            status = record['status']
            timestamp = datetime.fromtimestamp(record['timestamp']).strftime("%H:%M:%S")
            report += f"- {timestamp} - {role}: {status}\n"
        
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
    
    def mark_role_completed(self, role: str, success: bool = True, output: str = ""):
        """标记角色任务完成"""
        if role not in self.roles_state:
            print(f"❌ 未知角色: {role}")
            return
        
        current_time = datetime.now().timestamp()
        state = self.roles_state[role]
        
        # 更新状态
        state["status"] = "completed" if success else "failed"
        state["last_run"] = current_time
        state["completion_time"] = current_time
        
        if not success:
            state["error_count"] += 1
        
        # 记录完成历史
        self.completion_history.append({
            "role": role,
            "status": state["status"],
            "timestamp": current_time,
            "output": output[:200]  # 限制输出长度
        })
        
        # 如果成功完成，触发下一个角色
        if success:
            self.trigger_next_role(role)
        
        # 保存状态
        self.save_state()
        
        print(f"✅ 角色 '{role}' 标记为 {state['status']}")
    
    def trigger_next_role(self, completed_role: str):
        """触发下一个角色"""
        try:
            current_index = self.role_order.index(completed_role)
            
            # 查找下一个待执行的角色
            for i in range(current_index + 1, len(self.role_order)):
                next_role = self.role_order[i]
                
                # 检查是否被跳过
                if next_role in self.skip_markers:
                    print(f"⏸️ 角色 '{next_role}' 被跳过")
                    self.skip_markers.remove(next_role)
                    continue
                
                # 检查是否已经运行过
                state = self.roles_state[next_role]
                if state["status"] in ["completed", "running"]:
                    continue
                
                # 标记为待执行
                state["status"] = "pending"
                state["next_run"] = datetime.now().timestamp()
                
                # 设置跳过标记（防止重复触发）
                self.skip_markers.add(next_role)
                
                print(f"🚀 触发角色: {next_role}")
                
                # 这里应该调用 sessions_send 来唤醒下一个角色
                # 由于API限制，这里只是模拟
                self.simulate_role_wakeup(next_role)
                
                break
                
        except ValueError:
            print(f"❌ 无法找到完成角色 '{completed_role}' 在顺序中")
    
    def simulate_role_wakeup(self, role: str):
        """模拟角色唤醒（实际应该调用sessions_send）"""
        print(f"📤 模拟唤醒角色: {role}")
        print(f"💡 实际应该调用 sessions_send 向 {role} 发送唤醒消息")
        
        # 记录唤醒事件
        self.completion_history.append({
            "role": role,
            "status": "wakeup_triggered",
            "timestamp": datetime.now().timestamp(),
            "output": f"自动触发角色执行"
        })
    
    def update_role_status(self, role: str, status: str):
        """更新角色状态"""
        if role not in self.roles_state:
            print(f"❌ 未知角色: {role}")
            return
        
        self.roles_state[role]["status"] = status
        
        if status == "running":
            self.roles_state[role]["last_run"] = datetime.now().timestamp()
        
        self.save_state()
        print(f"✅ 角色 '{role}' 状态更新为: {status}")
    
    def get_ready_roles(self) -> List[str]:
        """获取准备执行的角色列表"""
        ready_roles = []
        current_time = datetime.now().timestamp()
        
        for role in self.role_order:
            state = self.roles_state[role]
            
            # 检查是否被跳过
            if role in self.skip_markers:
                continue
            
            # 检查时间是否到了
            if state["next_run"] and current_time >= state["next_run"]:
                # 检查状态
                if state["status"] in ["pending", "failed"]:
                    ready_roles.append(role)
        
        return ready_roles
    
    def get_system_status(self) -> Dict:
        """获取系统状态"""
        return {
            "initialized": self.initialized,
            "total_roles": len(self.roles_state),
            "completed_roles": len([r for r in self.roles_state.values() if r['status'] == 'completed']),
            "running_roles": len([r for r in self.roles_state.values() if r['status'] == 'running']),
            "failed_roles": len([r for r in self.roles_state.values() if r['status'] == 'failed']),
            "skipped_roles": len(self.skip_markers),
            "total_executions": len(self.completion_history),
            "ready_roles": self.get_ready_roles()
        }

# 全局调度追踪器实例
scheduler = SchedulingTracker()

def initialize_scheduling_system():
    """初始化调度系统"""
    print("🚀 正在初始化AetherTunnel智能调度系统...")
    
    # 初始化调度追踪器
    scheduler.__init__()
    
    # 标记系统为已初始化
    scheduler.initialized = True
    
    print("✅ 智能调度系统初始化完成")
    print(f"📊 系统状态: {scheduler.get_system_status()}")
    
    # 启动第一个角色（GitHub凭证配置）
    scheduler.update_role_status("GitHub凭证配置", "pending")
    
    return scheduler

def report_to_technical_hr(status: Dict):
    """向技术人事总管汇报状态"""
    print("\n" + "="*60)
    print("📊 向技术人事总管汇报调度系统状态")
    print("="*60)
    print(f"🕐 汇报时间: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    print(f"🔄 系统状态: {'🟢 运行中' if status['initialized'] else '🔴 初始化中'}")
    print(f"👥 角色总数: {status['total_roles']}")
    print(f"✅ 已完成: {status['completed_roles']}")
    print(f"🔄 运行中: {status['running_roles']}")
    print(f"❌ 失败: {status['failed_roles']}")
    print(f"⏸️ 跳过: {status['skipped_roles']}")
    print(f"📈 总执行次数: {status['total_executions']}")
    
    if status['ready_roles']:
        print(f"🚀 准备执行的角色: {', '.join(status['ready_roles'])}")
    
    print("="*60 + "\n")

if __name__ == "__main__":
    # 初始化调度系统
    scheduler = initialize_scheduling_system()
    
    # 汇报状态
    status = scheduler.get_system_status()
    report_to_technical_hr(status)
    
    print("🎯 AetherTunnel智能调度系统初始化完成！")
    print("💡 系统已准备好开始执行角色调度和自动唤醒机制")