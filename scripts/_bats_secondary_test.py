import io, pathlib, runpy, tempfile, types, unittest, unittest.mock
import yaml
from unittest.mock import Mock

G = runpy.run_path(str(pathlib.Path(__file__).with_name('bats')), run_name='bats_test_module')
BATS = G['BATS']


def primary():
    return {
        'network_cidr': '10.254.48.0/20', 'network_gateway': '10.254.0.1', 'network_bridge': 'vmbr0',
        'network_reserved': ['10.254.0.1-10.254.48.19'], 'network_static': ['10.254.48.190-10.254.48.199'],
        'static_ip': '10.254.48.190', 'second_static_ip': '10.254.48.191',
        'network_cidr_ipv6': '', 'network_gateway_ipv6': '', 'network_reserved_ipv6': [],
        'network_static_ipv6': [], 'static_ip_ipv6': '', 'nic_group': '1',
        'vm_cores': 2, 'vm_memory_mib': 2048, 'vm_disk_mib': 8192,
        'cpi_id': 'pve-az1', 'target_node': 'lab-pve-cpi-0', 'exclude_tags': [], 'rspec_timeout_s': 60,
        'secondary': None,
    }


def block(**over):
    value = {
        'network_cidr': '10.255.0.0/16', 'network_gateway': '10.255.0.1',
        'network_reserved': ['10.255.0.1-10.255.48.19'], 'network_static': ['10.255.48.190-10.255.48.199'],
        'static_ip': '10.255.48.190', 'second_static_ip': '10.255.48.191',
    }
    value.update(over)
    return value


class SecondaryConfigTests(unittest.TestCase):
    def resolve(self, raw, primary_id='pve-az1'):
        section = {} if raw is None else {'secondary': raw}
        return G['secondary_bats_config'](section, primary_id)

    def test_absent_or_empty_block_is_none(self):
        self.assertIsNone(self.resolve(None))
        self.assertIsNone(self.resolve({}))

    def test_defaults_to_pve_az2(self):
        resolved = self.resolve(block())
        self.assertEqual(resolved['cpi_id'], 'pve-az2')
        self.assertEqual(resolved['target_node'], '')

    def test_missing_required_key_exits(self):
        for key in ('network_cidr', 'network_gateway', 'network_reserved', 'network_static',
                    'static_ip', 'second_static_ip'):
            raw = block()
            del raw[key]
            with self.assertRaises(SystemExit) as ctx:
                self.resolve(raw)
            self.assertIn(f'bats.secondary.{key}', str(ctx.exception))

    def test_rejects_ipv6_and_inherited_tuning_keys(self):
        for key in ('network_cidr_ipv6', 'nic_group', 'exclude_tags', 'vm_cores'):
            with self.assertRaises(SystemExit):
                self.resolve(block(**{key: 'x'}))

    def test_rejects_inconsistent_bands_before_any_pass(self):
        bad = [
            dict(network_cidr='10.255.48.0/20'),
            dict(network_gateway='10.9.0.1'),
            dict(network_static=['10.255.48.10-10.255.48.30']),
            dict(static_ip='10.255.1.1'),
            dict(second_static_ip='10.255.1.2'),
            dict(network_gateway='not-an-ip'),
            dict(network_reserved=['10.255.0.1-zzz']),
        ]
        for over in bad:
            with self.assertRaises(SystemExit, msg=str(over)):
                self.resolve(block(**over))

    def test_rejects_same_cpi_id_scalar_lists_and_equal_ips(self):
        with self.assertRaises(SystemExit):
            self.resolve(block(cpi_id='pve-az1'))
        with self.assertRaises(SystemExit):
            self.resolve(block(network_static='10.255.48.190'))
        with self.assertRaises(SystemExit):
            self.resolve(block(second_static_ip='10.255.48.190'))
        with self.assertRaises(SystemExit):
            self.resolve(['not', 'a', 'mapping'])


class SecondarySpecTests(unittest.TestCase):
    def spec(self, bcfg):
        return G['build_bat_spec'](bcfg, 'stemcell', '1', 'pub', 'priv')

    def test_secondary_spec_carries_second_entry_values_only(self):
        bcfg = primary()
        bcfg['secondary'] = G['secondary_bats_config']({'secondary': block()}, 'pve-az1')
        merged = G['secondary_pass_config'](bcfg)
        props = self.spec(merged)['properties']
        self.assertEqual(props['cpi_id'], 'pve-az2')
        self.assertNotIn('target_node', props)
        self.assertEqual(props['networks'][0]['cidr'], '10.255.0.0/16')
        self.assertEqual(props['networks'][0]['cloud_properties']['bridge'], 'vmbr0')
        self.assertEqual(props['networks'][0]['static_ip'], '10.255.48.190')
        self.assertEqual(len(props['networks']), 1)
        net = props['networks'][0]
        self.assertEqual(net['gateway'], '10.255.0.1')
        self.assertEqual(net['reserved'], ['10.255.0.1-10.255.48.19'])
        self.assertEqual(net['static'], ['10.255.48.190-10.255.48.199'])
        self.assertNotIn('10.254', yaml.safe_dump(props))
        self.assertEqual(props['second_static_ip'], '10.255.48.191')
        self.assertEqual(props['vm_cores'], 2)
        # the primary spec is untouched
        base = self.spec(bcfg)['properties']
        self.assertEqual(base['cpi_id'], 'pve-az1')
        self.assertEqual(base['target_node'], 'lab-pve-cpi-0')
        self.assertEqual(base['networks'][0]['cidr'], '10.254.48.0/20')

    def test_no_block_means_no_secondary_config(self):
        self.assertIsNone(G['secondary_pass_config'](primary()))


class SecondaryPassTests(unittest.TestCase):
    def runner(self, secondary, skip=False, fail_first=False, fail_fast=False, only=None):
        value = BATS.__new__(BATS)
        bcfg = primary()
        if secondary:
            bcfg['secondary'] = G['secondary_bats_config']({'secondary': block()}, 'pve-az1')
        value.bcfg = bcfg
        value.args = types.SimpleNamespace(skip_secondary=skip, fail_fast=fail_fast, only=only or [],
                                           no_default_excludes=False, exclude=[])
        value.run_dir = pathlib.Path('/run')
        value.checkout = pathlib.Path('/co')
        value.rep = Mock()
        value.calls = []

        def one(name, dual, only_, junit, spec_path=None):
            value.calls.append((name, junit.name, spec_path.name if spec_path else None))
            return not (fail_first and name != 'secondary')
        value._run_rspec_pass = one
        return value

    def test_without_block_run_is_single_pass(self):
        run = self.runner(False)
        self.assertTrue(run.run_rspec())
        self.assertEqual(run.calls, [('ipv4', 'junit.xml', None)])

    def test_block_adds_secondary_pass_with_own_spec_and_junit(self):
        run = self.runner(True)
        self.assertTrue(run.run_rspec())
        self.assertEqual(run.calls, [('ipv4', 'junit.xml', None),
                                     ('secondary', 'junit-secondary.xml', 'bat-secondary.yml')])

    def test_skip_flag_and_dual_stack_selection_drop_the_pass(self):
        run = self.runner(True, skip=True)
        run.run_rspec()
        self.assertEqual(len(run.calls), 1)
        run = self.runner(True, only=['ipv6'])
        run.dual_stack_enabled = lambda: True
        run.run_rspec()
        self.assertEqual(len(run.calls), 1)

    def test_primary_failure_still_runs_secondary_unless_fail_fast(self):
        run = self.runner(True, fail_first=True)
        self.assertFalse(run.run_rspec())
        self.assertEqual(len(run.calls), 2)
        run = self.runner(True, fail_first=True, fail_fast=True)
        self.assertFalse(run.run_rspec())
        self.assertEqual(len(run.calls), 1)

    def test_secondary_only_failure_fails_the_run(self):
        run = self.runner(True)
        run._run_rspec_pass = lambda name, *a, **k: name != 'secondary'
        self.assertFalse(run.run_rspec())
        self.assertEqual(run.rspec_rc, 1)

    def test_secondary_meta_records_each_state(self):
        self.assertEqual(self.runner(False)._secondary_meta(), '')
        run = self.runner(True)
        run.run_rspec()
        self.assertIn('pve-az2 on 10.255.0.0/16', run._secondary_meta())
        run = self.runner(True, skip=True)
        run.run_rspec()
        self.assertIn('skipped', run._secondary_meta())
        run = self.runner(True, only=['ipv6'])
        run.dual_stack_enabled = lambda: True
        run.run_rspec()
        self.assertIn('not run', run._secondary_meta())


class SecondaryRenderTests(unittest.TestCase):
    """_prepare_bat_yml writes bat-secondary.yml at 0600, and only when the pass is wanted."""

    def render(self, skip):
        run = SecondaryPassTests().runner(True, skip=skip)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        run.run_dir = pathlib.Path(tmp.name)
        (run.run_dir / 'bats_ssh').write_text('priv')
        (run.run_dir / 'bats_ssh.pub').write_text('pub')
        run.stemcell_name, run.stemcell_version = 'stemcell', '1'
        run.dry_run = False
        run._record_inline = lambda *a, **k: True
        self.assertTrue(run._prepare_bat_yml())
        return run.run_dir

    def test_secondary_spec_is_written_private_with_second_entry_values(self):
        run_dir = self.render(skip=False)
        path = run_dir / 'bat-secondary.yml'
        self.assertEqual(path.stat().st_mode & 0o777, 0o600)
        props = yaml.safe_load(path.read_text())['properties']
        self.assertEqual(props['cpi_id'], 'pve-az2')
        self.assertEqual(props['networks'][0]['cidr'], '10.255.0.0/16')
        self.assertEqual(yaml.safe_load((run_dir / 'bat.yml').read_text())['properties']['cpi_id'], 'pve-az1')

    def test_skip_secondary_writes_no_secondary_spec(self):
        self.assertFalse((self.render(skip=True) / 'bat-secondary.yml').exists())


class ExcludeHelperTests(unittest.TestCase):
    def test_helper_anchors_and_escapes_each_description(self):
        text = G['render_exclude_helper'](["a.b (c) [d] it's \\ #{x}"])
        self.assertIn("RSpec.configure do |c|", text)
        self.assertIn("filter_run_excluding full_description:", text)
        self.assertIn("Regexp.escape('a.b (c) [d] it\\'s \\\\ #{x}')", text)
        self.assertIn("'\\A' + Regexp.escape(", text)
        self.assertIn("+ '\\z')", text)

    def test_helper_unions_several_descriptions_in_one_call(self):
        text = G['render_exclude_helper'](['one', 'two'])
        self.assertEqual(text.count('filter_run_excluding'), 1)
        self.assertIn('Regexp.union(', text)
        self.assertIn("Regexp.escape('one')", text)
        self.assertIn("Regexp.escape('two')", text)

    def test_table_names_the_delete_vm_reference_example(self):
        table = G['SECONDARY_EXCLUDED_EXAMPLES']
        desc = 'cck unresponsive agent delete_vm_reference should delete vm reference'
        self.assertIn(desc, table)
        self.assertIn('default CPI', table[desc])
        self.assertIn('primary pass runs this example', table[desc])
        self.assertIn('docs/operations.md', table[desc])


class SecondaryCommandTests(unittest.TestCase):
    """_run_rspec_pass builds the argv; the subprocess layer is a mock."""

    def pass_argv(self, name, spec_path=None):
        run = SecondaryPassTests().runner(True)
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        run.run_dir = pathlib.Path(tmp.name)
        run.dry_run = False
        run.stemcell_path = pathlib.Path('/stemcell.tgz')
        run.args.keep = False
        run._bundle_env = lambda: {}
        run._director_env = lambda: {}
        run.pass_timeout_s = 60
        run.runner = Mock()
        run.runner.step.return_value = (True, '')
        del run.__dict__['_run_rspec_pass']
        self.assertTrue(run._run_rspec_pass(name, False, [], run.run_dir / 'junit.xml', spec_path=spec_path))
        return run, run.runner.step.call_args[0][1]

    def test_secondary_pass_requires_the_helper_and_writes_it(self):
        run, argv = self.pass_argv('secondary', pathlib.Path('/x/bat-secondary.yml'))
        helper = run.run_dir / 'rspec-exclude-secondary.rb'
        self.assertEqual(argv[argv.index(str(helper)) - 1], '--require')
        self.assertIn('delete_vm_reference', helper.read_text())

    def test_primary_pass_does_not_require_the_helper(self):
        run, argv = self.pass_argv('ipv4')
        self.assertFalse(any('rspec-exclude-secondary' in a for a in argv))
        self.assertFalse((run.run_dir / 'rspec-exclude-secondary.rb').exists())

    def test_report_lists_excluded_example_with_reason(self):
        run = SecondaryPassTests().runner(True)
        run.run_rspec()
        self.assertEqual(run.excluded_examples, G['SECONDARY_EXCLUDED_EXAMPLES'])
        meta = {'excluded_examples': run.excluded_examples}
        self.assertIn('delete_vm_reference', ' '.join(meta['excluded_examples']))
        run = SecondaryPassTests().runner(True, skip=True)
        run.excluded_examples = {}
        run.run_rspec()
        self.assertEqual(run.excluded_examples, {})

    def test_run_doc_has_example_exclusions_section(self):
        captured = {}
        desc = 'cck unresponsive agent delete_vm_reference should delete vm reference'
        meta = {
            'date': '2026-10-13', 'verdict': 'PASSED', 'excluded_tags': [], 'excluded_reasons': {},
            'excluded_examples': {desc: 'because | reason'}, 'wall_hms': '0', 'started': 's',
            'finished': 'f', 'command_line': 'c', 'exit_code': 0, 'secondary_pass': 'x',
            'primary_pass': 'skipped on request (--secondary-only)',
        }
        with unittest.mock.patch.object(
                G['_rundoc'], 'write_run_doc',
                lambda **kw: captured.update(kw) or pathlib.Path('/doc')):
            G['write_run_doc'](meta, None)
        sections = '\n'.join(captured['extra_sections'])
        self.assertIn('## Example exclusions', sections)
        self.assertIn(desc, sections)
        self.assertIn('because \\| reason', sections)
        verdict = '\n'.join(captured['verdict_lines'])
        self.assertIn('Primary pass: skipped on request', verdict)


class SecondaryOnlyTests(unittest.TestCase):
    def runner(self, **kw):
        run = SecondaryPassTests().runner(True, **kw)
        run.args.secondary_only = True
        return run

    def test_runs_only_the_secondary_pass(self):
        run = self.runner()
        self.assertTrue(run.run_rspec())
        self.assertEqual(run.calls, [('secondary', 'junit-secondary.xml', 'bat-secondary.yml')])
        self.assertEqual([p.name for p in run.junit_paths], ['junit-secondary.xml'])

    def test_failure_in_the_secondary_pass_fails_the_run(self):
        run = self.runner()
        run._run_rspec_pass = lambda *a, **k: False
        self.assertFalse(run.run_rspec())

    def test_report_meta_says_primary_was_skipped(self):
        run = self.runner()
        run.run_rspec()
        self.assertTrue(run._secondary_only())
        self.assertIn('pve-az2', run._secondary_meta())

    def test_missing_block_is_a_usage_error(self):
        args = types.SimpleNamespace(
            secondary_only=True, config='c', env='e', results_dir=tempfile.gettempdir(),
            dry_run=True, skip_doc=True,
        )
        with unittest.mock.patch.dict(G['BATS'].__init__.__globals__['_integration'].__dict__, {
            'load_config': lambda *a, **k: {},
        }), unittest.mock.patch.dict(G['BATS'].__init__.__globals__, {
            'bats_config': lambda *a, **k: primary(),
        }):
            with self.assertRaises(SystemExit) as ctx:
                G['BATS'](args, Mock(run_dir=lambda d: pathlib.Path(d)))
        self.assertIn('--secondary-only needs a bats.secondary block', str(ctx.exception))

    def test_flag_combinations_are_usage_errors(self):
        main = G['main']
        for extra in (['--skip-secondary'], ['--only', 'ipv6'], ['--only', 'dual_stack']):
            with self.assertRaises(SystemExit) as ctx, \
                    unittest.mock.patch('sys.stderr', new=io.StringIO()):
                main(['run', '--secondary-only'] + extra)
            self.assertEqual(ctx.exception.code, 2)


if __name__ == '__main__':
    unittest.main()
