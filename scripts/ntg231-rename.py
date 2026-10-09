#!/usr/bin/env python3
"""NTG-231 rollback, rename step. Run as root, after the lab is destroyed with cleanup=false.

For every cisco_iol node of the topology, move <labdir>/clab-<lab>/<node>/nvram back to
nvram_<Index+1>, the file a pre-NTG-231 containerlab opens (Index = place among ALL node names,
sorted, like core/config.go). An existing target is kept as <target>.pre-rollback.
Usage: ntg231-rename.py <topology.clab.yml> [--dry-run]
"""
import os, sys, yaml

topo_path, dry = sys.argv[1], '--dry-run' in sys.argv
topo = yaml.safe_load(open(topo_path))
lab = topo['name']
nodes = topo['topology']['nodes']
labdir = os.path.join(os.path.dirname(os.path.abspath(topo_path)), f'clab-{lab}')
names = sorted(nodes)
for index, name in enumerate(names):
    kind = (nodes[name] or {}).get('kind') or topo['topology'].get('defaults', {}).get('kind')
    if kind != 'cisco_iol':
        continue
    d = os.path.join(labdir, name)
    src, dst = os.path.join(d, 'nvram'), os.path.join(d, f'nvram_{index + 1:05d}')
    if not os.path.isfile(src) or os.path.getsize(src) <= 1:
        print(f'{name}: no written nvram, left as is')
        continue
    print(f'{name}: nvram -> {os.path.basename(dst)}' + (' (existing kept as .pre-rollback)' if os.path.exists(dst) else ''))
    if dry:
        continue
    if os.path.exists(dst):
        os.replace(dst, dst + '.pre-rollback')
    os.replace(src, dst)
known = {n for n in names}
for extra in sorted(e for e in set(os.listdir(labdir)) - known if os.path.isdir(os.path.join(labdir, e)) and not e.startswith('.')) if os.path.isdir(labdir) else []:
    print(f'skipped {extra}: not in the topology')
