#!/usr/bin/env python3
"""
AetherTunnel 配置文件测试脚本
验证所有 TOML 配置文件的语法正确性
"""

import sys
import os
from pathlib import Path

try:
    import tomllib
except ImportError:
    try:
        import tomli as tomllib
    except ImportError:
        print("Error: Need tomllib or tomli to parse TOML files")
        print("Install: pip install tomli")
        sys.exit(1)

def test_toml_file(filepath):
    """测试单个 TOML 文件"""
    try:
        with open(filepath, 'rb') as f:
            data = tomllib.load(f)
        return True, None, data
    except Exception as e:
        return False, str(e), None

def main():
    """主测试函数"""
    aethertunnel_dir = Path('/workspace/projects/workspace/aethertunnel')

    if not aethertunnel_dir.exists():
        print(f"❌ AetherTunnel 目录不存在: {aethertunnel_dir}")
        return False

    print("🔍 开始测试 AetherTunnel 配置文件...")
    print("=" * 60)

    # 测试的配置文件列表
    config_files = [
        'server.toml.example',
        'client.toml.example',
        'server-toml-innovative-addon.example',
        'client-toml-innovative-addon.example',
        'dashboard-full-config.example',
        'dashboard-quick-config.example',
    ]

    all_passed = True
    results = []

    for config_file in config_files:
        filepath = aethertunnel_dir / config_file

        if not filepath.exists():
            print(f"⚠️  文件不存在: {config_file}")
            continue

        print(f"\n📄 测试: {config_file}")
        print("-" * 60)

        success, error, data = test_toml_file(filepath)

        if success:
            print(f"✅ 通过 - 解析成功")
            print(f"   顶层节点数量: {len(data)}")

            # 显示顶层节点
            for key in data.keys():
                print(f"   - {key}")

            results.append((config_file, True, None))
        else:
            print(f"❌ 失败 - {error}")
            results.append((config_file, False, error))
            all_passed = False

    # 测试报告
    print("\n" + "=" * 60)
    print("📊 测试报告")
    print("=" * 60)

    passed = sum(1 for _, success, _ in results if success)
    total = len(results)

    print(f"总测试数: {total}")
    print(f"通过: {passed}")
    print(f"失败: {total - passed}")

    if all_passed:
        print("\n✅ 所有配置文件测试通过！")
        return True
    else:
        print("\n❌ 部分配置文件测试失败！")
        print("\n失败详情:")
        for filename, success, error in results:
            if not success:
                print(f"  - {filename}: {error}")
        return False

if __name__ == '__main__':
    success = main()
    sys.exit(0 if success else 1)
