#!/usr/bin/env python3
"""Offline checks of input fidelity against Swagger and reviewed overrides."""
import copy
import importlib.util
import json
import pathlib
import re
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
module = importlib.util.spec_from_file_location('generator', ROOT / 'scripts/generate-api-tools.py')
generator = importlib.util.module_from_spec(module)
module.loader.exec_module(generator)


class CatalogContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.source = json.loads((ROOT / 'doc.json').read_text(encoding='utf-8-sig'))
        cls.overrides = json.loads((ROOT / 'scripts/api-overrides.json').read_text(encoding='utf-8'))
        catalog = json.loads((ROOT / 'internal/toolset/catalog/catalog.json').read_text(encoding='utf-8'))
        cls.operations = {(r['operation']['method'], r['operation']['path'][7:]): r['operation']
                          for r in catalog['tools']}

    def assert_constraints_preserved(self, expected, actual):
        if isinstance(expected, dict):
            self.assertIsInstance(actual, dict)
            for key, value in expected.items():
                if not key.startswith('x-'):
                    self.assertIn(key, actual)
                    self.assert_constraints_preserved(value, actual[key])
        elif isinstance(expected, list):
            self.assertEqual(len(expected), len(actual))
            for left, right in zip(expected, actual):
                self.assert_constraints_preserved(left, right)
        else:
            self.assertEqual(expected, actual)

    def test_all_parameter_constraints_and_referenced_definitions(self):
        checked = 0
        for path, item in self.source['paths'].items():
            for method, declared in item.items():
                if method not in generator.METHODS:
                    continue
                key = method.upper() + ' ' + path
                with self.subTest(operation=key):
                    route = re.sub(r'\{([^}]+)\}', r':\1', path)
                    operation = self.operations[(method.upper(), route)]
                    schema = operation['bodySchema']
                    correction = self.overrides.get(key, {})
                    parameters = correction.get('parameters', item.get('parameters', []) + declared.get('parameters', []))
                    parameters = parameters + correction.get('extraParameters', [])
                    for param in parameters:
                        checked += 1
                        location, name = param['in'], param['name']
                        if location == 'query' and name == 'operateNode':
                            self.assertTrue(operation['transport']['nodeQuery'])
                            self.assertNotIn(name, schema['properties'].get('query', {}).get('properties', {}))
                            continue
                        group = {'body':'body', 'path':'path', 'query':'query', 'formData':'form', 'header':'headers'}[location]
                        actual = schema['properties'][group]
                        required = param.get('required', False) or location == 'path'
                        if required:
                            self.assertIn(group, schema['required'])
                        if location == 'body':
                            self.assert_constraints_preserved(param.get('schema', {}), actual)
                            continue
                        if required:
                            self.assertIn(name, actual['required'])
                        actual = actual['properties'][name]
                        if param.get('type') == 'file':
                            self.assertIn(name, operation['transport']['fileFields'])
                            self.assertEqual(set(actual['required']), {'filename', 'data'})
                        else:
                            constraints = {k:v for k,v in param.items() if k not in ('in', 'name', 'required', 'collectionFormat')}
                            self.assert_constraints_preserved(constraints, actual)
                    for name, definition in schema.get('definitions', {}).items():
                        self.assert_constraints_preserved(self.source['definitions'][name], definition)
        self.assertGreater(checked, 500)

    def test_generation_does_not_mutate_source_or_overrides(self):
        source, overrides = copy.deepcopy(self.source), copy.deepcopy(self.overrides)
        # Exercise replacement and extension together, which used to mutate the
        # override and duplicate the added parameter on a second generation.
        overrides['POST /files/chunkdownload']['extraParameters'] = [
            {'in':'query', 'name':'test', 'type':'string'}]
        before_source, before_overrides = copy.deepcopy(source), copy.deepcopy(overrides)
        first = generator.generate(source, overrides)
        self.assertEqual(source, before_source)
        self.assertEqual(overrides, before_overrides)
        self.assertEqual(first, generator.generate(source, overrides))

    def test_unsupported_wire_contracts_fail_generation(self):
        source = {'swagger':'2.0', 'basePath':'/api/v2', 'definitions':{}, 'paths':{
            '/example':{'post':{'tags':['Example'], 'summary':'Example'}}}}
        for correction in [
            {'parameters':[{'in':'query','name':'ids','type':'array','items':{'type':'integer'},'collectionFormat':'multi'}]},
            {'parameters':[{'in':'header','name':'Authorization','type':'string'}]},
            {'parameters':[{'in':'query','name':'id','type':'string'}] * 2},
            {'parameters':[{'in':'body','name':'request','schema':{}}, {'in':'formData','name':'file','type':'file'}]},
            {'kind':'unsupported'},
            {'response':'unsupported'},
            {'typo':'binary'},
        ]:
            with self.subTest(correction=correction), self.assertRaises(ValueError):
                generator.generate(source, {'POST /example':dict(correction, source='test fixture')})


if __name__ == '__main__':
    unittest.main()
