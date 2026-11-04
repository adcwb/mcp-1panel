#!/usr/bin/env python3
"""Generate the compiled API catalog and coverage inventory from 1Panel Swagger.

No network, credentials or running panel are needed. --check detects spec drift.
Explicit corrections live in api-overrides.json with their upstream evidence.
"""
import argparse
import copy
import hashlib
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
METHODS = {'get', 'post', 'put', 'patch', 'delete', 'head', 'options'}


def object_schema(properties=None, required=None):
    result = {'type': 'object', 'properties': properties or {}, 'additionalProperties': False}
    if required:
        result['required'] = sorted(required)
    return result


def slug(value):
    return re.sub('[^a-z0-9]+', '_', value.lower()).strip('_')


def file_schema():
    return object_schema({
        'filename': {'type': 'string', 'minLength': 1, 'maxLength': 255,
                     'pattern': r'^[^/\\\r\n\x00]+$'},
        'data': {'type': 'string', 'maxLength': 900000,
                 'description': 'Base64 file contents; no local filesystem paths are opened.'},
    }, ['filename', 'data'])


def normalize(value):
    """Keep JSON Schema constraints, including maps, recursion and numeric enums."""
    if not isinstance(value, dict):
        return value
    result = {}
    for key, item in value.items():
        if key.startswith('x-'):
            continue
        if key in ('properties', 'definitions'):
            result[key] = {k: normalize(v) for k, v in item.items()}
        elif key in ('items', 'additionalProperties') and isinstance(item, dict):
            result[key] = normalize(item)
        elif key in ('allOf', 'anyOf', 'oneOf'):
            result[key] = [normalize(v) for v in item]
        else:
            result[key] = item
    if result.get('type') == 'object' and 'properties' in result:
        result.setdefault('additionalProperties', False)
    return result


def references(value):
    if isinstance(value, dict):
        if '$ref' in value:
            yield value['$ref']
        for item in value.values():
            yield from references(item)
    elif isinstance(value, list):
        for item in value:
            yield from references(item)


def add_definitions(schema, definitions):
    pending = list(references(schema))
    included = {}
    while pending:
        ref = pending.pop()
        if not ref.startswith('#/definitions/'):
            raise ValueError('Only local references are supported: ' + ref)
        name = ref[len('#/definitions/'):]
        if name not in included:
            included[name] = normalize(definitions[name])
            pending.extend(references(included[name]))
    if included:
        schema['definitions'] = included


def generate(source, overrides):
    if source.get('swagger') != '2.0' or source.get('basePath') != '/api/v2':
        raise ValueError('Expected a 1Panel v2 Swagger 2.0 document')
    tags = sorted({op['tags'][0] for item in source['paths'].values()
                   for method, op in item.items() if method in METHODS})
    records, names, seen_overrides = [], set(), set()
    for route, item in sorted(source['paths'].items()):
        for method, original in sorted(item.items()):
            if method not in METHODS:
                continue
            key = method.upper() + ' ' + route
            correction = overrides.get(key, {})
            if correction:
                seen_overrides.add(key)
                if not correction.get('source'):
                    raise ValueError('Override requires source evidence: ' + key)
                unknown = set(correction) - {'source', 'reason', 'parameters', 'extraParameters', 'kind', 'response'}
                if unknown:
                    raise ValueError('Unknown override fields: ' + repr(unknown))
            op = copy.deepcopy(original)
            parameters = item.get('parameters', []) + op.get('parameters', [])
            if 'parameters' in correction:
                parameters = copy.deepcopy(correction['parameters'])
            parameters += correction.get('extraParameters', [])
            groups, required_groups, files = {}, [], []
            query_node = False
            seen_parameters = set()
            for p in parameters:
                location, name = p['in'], p['name']
                identity = (location, name)
                if identity in seen_parameters:
                    raise ValueError('Duplicate parameter in ' + key + ': ' + repr(identity))
                seen_parameters.add(identity)
                if location != 'body' and p.get('type') not in ('string', 'integer', 'boolean', 'file'):
                    raise ValueError('Unsupported scalar parameter in ' + key + ': ' + name)
                if location == 'header' and name not in ('EntranceCode', 'Range'):
                    raise ValueError('Unsupported header in ' + key + ': ' + name)
                if location == 'query' and name == 'operateNode':
                    query_node = True
                    continue
                if location == 'body':
                    if 'body' in groups:
                        raise ValueError('Multiple body parameters in ' + key)
                    groups['body'] = normalize(p.get('schema', {}))
                    if p.get('required'):
                        required_groups.append('body')
                    continue
                group = {'path': 'path', 'query': 'query', 'header': 'headers', 'formData': 'form'}[location]
                schema = {k: v for k, v in p.items() if k not in
                          ('in', 'name', 'required', 'collectionFormat')}
                if p.get('type') == 'file':
                    schema = file_schema()
                    files.append(name)
                container = groups.setdefault(group, object_schema())
                container['properties'][name] = normalize(schema)
                if p.get('required') or location == 'path':
                    container.setdefault('required', []).append(name)
                    required_groups.append(group)
            path = re.sub(r'\{([^}]+)\}', r':\1', route)
            for name in re.findall(r':([^/]+)', path):
                container = groups.setdefault('path', object_schema())
                container['properties'].setdefault(name, {'type': 'string', 'minLength': 1})
                if name not in container.setdefault('required', []):
                    container['required'].append(name)
                required_groups.append('path')
            # Undocumented POST inputs remain explicit JSON values, constrained by
            # the fixed endpoint and full-access policy; never invent a DTO.
            if method in ('post', 'put', 'patch', 'delete') and 'body' not in groups and 'form' not in groups:
                groups['body'] = {'description': 'Optional JSON body; this endpoint has no request schema in Swagger.'}
            kind = correction.get('kind', 'http')
            if kind not in ('http', 'websocket', 'sse') or correction.get('response', 'json') not in ('json', 'binary'):
                raise ValueError('Unsupported transport in ' + key)
            if 'body' in groups and 'form' in groups:
                raise ValueError('Mixed body/form parameters in ' + key)
            if kind in ('websocket', 'sse'):
                groups['stream'] = object_schema({
                    'messages': {'type': 'array', 'maxItems': 100, 'items': object_schema({
                        'type': {'type': 'string', 'enum': ['text', 'binary']},
                        'data': {'type': 'string', 'maxLength': 65536,
                                 'description': 'UTF-8 text, or base64 for a binary frame.'},
                    }, ['type', 'data'])},
                    'readTimeoutMs': {'type': 'integer', 'minimum': 1, 'maximum': 30000, 'default': 1000},
                    'maxMessages': {'type': 'integer', 'minimum': 1, 'maximum': 100, 'default': 20},
                })
                if kind == 'sse':
                    del groups['stream']['properties']['messages']
            schema = object_schema(groups, set(required_groups))
            add_definitions(schema, source['definitions'])
            name = 'api_' + method + '_' + slug(route.replace(':', 'by_').replace('{', 'by_'))
            if len(name) > 64:
                name = name[:53] + '_' + hashlib.sha256(key.encode()).hexdigest()[:10]
            if name in names:
                raise ValueError('Duplicate tool name: ' + name)
            names.add(name)
            tag = op['tags'][0]
            transport = {'kind': kind, 'encoding': 'multipart' if 'form' in groups else 'json'}
            if files:
                transport['fileFields'] = sorted(files)
            if query_node:
                transport['nodeQuery'] = True
            if correction.get('response'):
                transport['response'] = correction['response']
            records.append({'name': name, 'summary': op['summary'], 'category': tag,
                            'categoryOrder': 100 + tags.index(tag),
                            'operation': {'id': 'api', 'method': method.upper(), 'path': '/api/v2' + path,
                                          'bodySchema': schema, 'responseFields': [], 'transport': transport}})
    if set(overrides) != seen_overrides:
        raise ValueError('Stale overrides: ' + repr(set(overrides) - seen_overrides))
    return records


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--spec', type=pathlib.Path, default=ROOT / 'doc.json')
    parser.add_argument('--check', action='store_true')
    args = parser.parse_args()
    raw = args.spec.read_bytes()
    source = json.loads(raw.decode('utf-8-sig'))
    overrides = json.loads((ROOT / 'scripts/api-overrides.json').read_text(encoding='utf-8'))
    records = generate(source, overrides)
    digest = hashlib.sha256(raw).hexdigest()
    catalog = '{"sourceSHA256":' + json.dumps(digest) + ',"tools":[\n'
    catalog += ',\n'.join(json.dumps(r, ensure_ascii=False, sort_keys=True, separators=(',', ':')) for r in records)
    catalog += '\n]}\n'
    lines = ['# 1Panel API coverage', '', 'Generated by `python scripts/generate-api-tools.py` from `doc.json`.',
             '', 'Source SHA-256: `' + digest + '`.', '',
             '{} documented operations; {} classified API tools. All require `full` access.'.format(len(records), len(records)),
             'Registration coverage is not live verification. Managed execution requires an endpoint-v1 capable Core.',
             'See [usage and limits](api-tools.md) and `scripts/api-overrides.json` for documented corrections.', '',
             '| Category | Tool | Method | Path | Transport |', '| --- | --- | --- | --- | --- |']
    for r in sorted(records, key=lambda r: (r['category'], r['name'])):
        op = r['operation']
        lines.append('| {} | `{}` | {} | `{}` | {} |'.format(r['category'], r['name'], op['method'], op['path'],
                     op['transport']['kind'] + '/' + op['transport']['encoding']))
    outputs = {ROOT / 'internal/toolset/catalog/catalog.json': catalog,
               ROOT / 'docs/api-coverage.md': '\n'.join(lines) + '\n'}
    for destination, content in outputs.items():
        if args.check:
            if not destination.exists() or destination.read_text(encoding='utf-8') != content:
                sys.exit('Generated file is stale: ' + str(destination))
        else:
            destination.parent.mkdir(parents=True, exist_ok=True)
            with destination.open('w', encoding='utf-8', newline='\n') as handle:
                handle.write(content)
    print('{} operations in {} categories; catalog {}'.format(len(records), len(set(r['category'] for r in records)),
          'verified' if args.check else 'generated'))


if __name__ == '__main__':
    main()
