#!/usr/bin/env python3
"""NTG-231 rollback, rename step. Run as root while the lab has no containers.

A containerlab built before NTG-231 opens <node>/nvram_<Index+1> (Index = the node's place among
ALL node names, sorted, as in core/config.go); NTG-231 keeps the saved config in <node>/nvram.
For every cisco_iol node with a written nvram (> 1 byte), move it back to nvram_<Index+1>.
An existing target is kept as <target>.pre-rollback-<timestamp>.
Usage: ntg231-rename.py <topology.clab.yml> [--dry-run]
"""
import os, sys, time, yaml

topo_path, dry = sys.argv[1], '--dry-run' in sys.argv
topo = yaml.safe_load(open(topo_path))
lab = topo['name']
t = topo.get('topology') or {}
nodes, groups, defaults = t.get('nodes') or {}, t.get('groups') or {}, t.get('defaults') or {}
labdir = os.path.join(os.path.dirname(os.path.abspath(topo_path)), f'clab-{lab}')
stamp = time.strftime('%Y%m%dT%H%M%S')


def kind_of(node):
    node = node or {}
    return node.get('kind') or (groups.get(node.get('group')) or {}).get('kind') or defaults.get('kind')


names = sorted(nodes)
for index, name in enumerate(names):
    if kind_of(nodes[name]) != 'cisco_iol':
        continue
    d = os.path.join(labdir, name)
    src, dst = os.path.join(d, 'nvram'), os.path.join(d, f'nvram_{index + 1:05d}')
    if not os.path.isfile(src) or os.path.getsize(src) <= 1:
        print(f'{lab}/{name}: no written nvram, left as is')
        continue
    backup = f'{dst}.pre-rollback-{stamp}'
    print(f'{lab}/{name}: nvram -> {os.path.basename(dst)}' + (f' (old target kept as {os.path.basename(backup)})' if os.path.exists(dst) else ''))
    if dry:
        continue
    if os.path.exists(dst):
        os.rename(dst, backup)
    os.rename(src, dst)
if os.path.isdir(labdir):
    for extra in sorted(set(os.listdir(labdir)) - set(names)):
        if os.path.isdir(os.path.join(labdir, extra)) and not extra.startswith('.'):
            print(f'{lab}: skipped {extra}, not in the topology')
