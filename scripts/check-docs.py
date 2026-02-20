#!/usr/bin/env python3
"""
AetherTunnel 文档检查脚本
检查文档完整性、链接有效性等
"""

import os
import re
from pathlib import Path
from urllib.parse import urlparse

def extract_markdown_links(filepath):
    """从 Markdown 文件中提取链接"""
    links = []
    with open(filepath, 'r', encoding='utf-8') as f:
        content = f.read()

    # 匹配 Markdown 链接 [text](url)
    pattern = r'\[([^\]]+)\]\(([^)]+)\)'
    matches = re.findall(pattern, content)

    for text, url in matches:
        links.append({'text': text, 'url': url, 'type': 'markdown'})

    # 匹配图片链接 <img src="url">
    pattern = r'<img[^>]+src=["\']([^"\']+)["\']'
    matches = re.findall(pattern, content)
    for url in matches:
        links.append({'text': 'image', 'url': url, 'type': 'image'})

    return links

def check_documentation(dirpath):
    """检查文档文件"""
    issues = []
    doc_files = []

    # 查找所有 Markdown 文件
    for md_file in dirpath.rglob('*.md'):
        doc_files.append(md_file)

    # 查找所有 README 文件
    for readme in dirpath.rglob('README*'):
        doc_files.append(readme)

    print(f"   找到 {len(doc_files)} 个文档文件")

    # 检查每个文档
    for doc_file in doc_files:
        relative_path = doc_file.relative_to(dirpath)

        # 检查文件大小
        size = doc_file.stat().st_size
        if size == 0:
            issues.append((relative_path, '文档文件为空'))
        elif size < 100:
            issues.append((relative_path, f'文档文件过小 ({size} bytes)'))

        # 提取并检查链接
        links = extract_markdown_links(doc_file)

        for link in links:
            url = link['url']

            # 跳过锚点链接
            if url.startswith('#'):
                continue

            # 检查相对路径链接
            if url.startswith('../') or url.startswith('./'):
                target_path = (doc_file.parent / url).resolve()
                if not target_path.exists():
                    issues.append((relative_path, f'链接不存在: {url}'))

    return issues

def check_config_files(dirpath):
    """检查配置文件"""
    issues = []
    config_files = list(dirpath.rglob('*.toml'))
    config_files += list(dirpath.rglob('*.example'))

    print(f"   找到 {len(config_files)} 个配置文件")

    for config_file in config_files:
        relative_path = config_file.relative_to(dirpath)

        # 检查文件大小
        size = config_file.stat().st_size
        if size == 0:
            issues.append((relative_path, '配置文件为空'))

        # 检查是否为 example 文件
        if 'example' not in config_file.name:
            issues.append((relative_path, '配置文件命名建议添加 .example 后缀'))

    return issues

def main():
    """主检查函数"""
    aethertunnel_dir = Path('/workspace/projects/workspace/aethertunnel')

    if not aethertunnel_dir.exists():
        print(f"❌ AetherTunnel 目录不存在: {aethertunnel_dir}")
        return False

    print("🔍 开始检查 AetherTunnel 文档...")
    print("=" * 60)

    all_issues = []

    # 检查文档文件
    print("\n📚 检查文档文件...")
    doc_issues = check_documentation(aethertunnel_dir)
    all_issues.extend(doc_issues)

    # 检查配置文件
    print("\n⚙️  检查配置文件...")
    config_issues = check_config_files(aethertunnel_dir)
    all_issues.extend(config_issues)

    # 检查必要文件
    print("\n📋 检查必要文件...")
    required_files = [
        ('go.mod', 'Go 模块文件'),
        ('README.md', '项目说明'),
        ('QUICK_START.md', '快速开始指南'),
    ]

    for filename, description in required_files:
        filepath = aethertunnel_dir / filename
        if not filepath.exists():
            all_issues.append((filename, f'{description}不存在'))

    # 检查目录结构
    print("\n📁 检查目录结构...")
    required_dirs = [
        'server',
        'client',
        'pkg',
        'docs',
        'scripts',
    ]

    for dirname in required_dirs:
        dirpath = aethertunnel_dir / dirname
        if not dirpath.exists():
            all_issues.append((dirname, '必要目录不存在'))

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
        return True

if __name__ == '__main__':
    import sys
    success = main()
    sys.exit(0 if success else 1)
