#!/usr/bin/env python3
"""
AetherTunnel 代码检查脚本
检查代码语法和基本问题
"""

import os
import sys
from pathlib import Path

def check_go_file_syntax(filepath):
    """检查 Go 文件的基本语法问题"""
    issues = []
    with open(filepath, 'r', encoding='utf-8') as f:
        lines = f.readlines()

    for i, line in enumerate(lines, 1):
        # 检查导入语句格式
        if 'import' in line and '("' not in line and '`"' not in line:
            if line.strip() != 'import (' and line.strip() != 'import' and ')' not in line:
                if line.strip() and not line.strip().startswith('//'):
                    issues.append(f"行 {i}: 可能的导入格式问题 - {line.strip()}")

        # 检查 package 语句
        if i == 1 and 'package' in line and not line.strip().startswith('package '):
            issues.append(f"行 {i}: package 语句格式可能错误 - {line.strip()}")

    return issues

def check_package_structure(dirpath):
    """检查包结构"""
    issues = []
    go_files = list(dirpath.glob('*.go'))

    if not go_files:
        return issues

    # 检查是否有 package 语句
    has_package = False
    for go_file in go_files:
        with open(go_file, 'r', encoding='utf-8') as f:
            first_line = f.readline()
            if first_line.startswith('package '):
                has_package = True
                break

    if not has_package:
        issues.append(f"{dirpath.name}: 缺少 package 语句")

    return issues

def main():
    """主检查函数"""
    aethertunnel_dir = Path('/workspace/projects/workspace/aethertunnel')

    if not aethertunnel_dir.exists():
        print(f"❌ AetherTunnel 目录不存在: {aethertunnel_dir}")
        return False

    print("🔍 开始检查 AetherTunnel 代码...")
    print("=" * 60)

    all_issues = []

    # 检查所有 Go 文件
    go_files = list(aethertunnel_dir.rglob('*.go'))
    print(f"\n📄 找到 {len(go_files)} 个 Go 文件")

    for go_file in go_files:
        relative_path = go_file.relative_to(aethertunnel_dir)
        issues = check_go_file_syntax(go_file)
        if issues:
            all_issues.extend([(relative_path, issue) for issue in issues])

    # 检查包结构
    print("\n📦 检查包结构...")
    pkg_dirs = [d for d in aethertunnel_dir.rglob('pkg/*') if d.is_dir()]
    pkg_dirs += [d for d in aethertunnel_dir.rglob('server') if d.is_dir()]
    pkg_dirs += [d for d in aethertunnel_dir.rglob('client') if d.is_dir()]

    for pkg_dir in set(pkg_dirs):
        relative_path = pkg_dir.relative_to(aethertunnel_dir)
        issues = check_package_structure(pkg_dir)
        if issues:
            all_issues.extend([(relative_path, issue) for issue in issues])

    # 检查 go.mod
    print("\n📋 检查 go.mod...")
    go_mod = aethertunnel_dir / 'go.mod'
    if go_mod.exists():
        with open(go_mod, 'r') as f:
            mod_content = f.read()

        if 'module ' not in mod_content:
            all_issues.append(('go.mod', '缺少 module 语句'))

        if 'go ' not in mod_content:
            all_issues.append(('go.mod', '缺少 go 版本语句'))

        if 'require' not in mod_content:
            all_issues.append(('go.mod', '缺少 require 语句'))
    else:
        all_issues.append(('go.mod', 'go.mod 文件不存在'))

    # 检查文档文件
    print("\n📚 检查文档文件...")
    required_docs = [
        'README.md',
        'QUICK_START.md',
        'server.toml.example',
        'client.toml.example',
    ]

    for doc in required_docs:
        doc_path = aethertunnel_dir / doc
        if not doc_path.exists():
            all_issues.append((doc, '文档文件不存在'))

    # 检查测试文件
    print("\n🧪 检查测试文件...")
    test_files = [f for f in go_files if f.name.endswith('_test.go')]
    print(f"   找到 {len(test_files)} 个测试文件")

    if len(test_files) == 0:
        print("   ⚠️  警告：没有找到测试文件")

    # 报告
    print("\n" + "=" * 60)
    print("📊 检查报告")
    print("=" * 60)

    if all_issues:
        print(f"\n❌ 发现 {len(all_issues)} 个问题：\n")
        for filepath, issue in all_issues:
            print(f"  - {filepath}: {issue}")
        return False
    else:
        print("\n✅ 所有检查通过！")
        print(f"   - Go 文件: {len(go_files)}")
        print(f"   - 测试文件: {len(test_files)}")
        print(f"   - 包目录: {len(pkg_dirs)}")
        return True

if __name__ == '__main__':
    success = main()
    sys.exit(0 if success else 1)
