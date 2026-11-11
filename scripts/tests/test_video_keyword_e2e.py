import json
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'scripts/test-video-e2e.sh'


def data_module():
    source = SCRIPT.read_text().split("<<'PY_VIDEO_DATA'\n", 1)[1].split('\nPY_VIDEO_DATA', 1)[0]
    namespace = {'__name__': 'video_data_test'}
    exec(compile(source, str(SCRIPT), 'exec'), namespace)
    namespace['free_port'] = lambda *args: 18081
    namespace['udp_range'] = lambda: (41000, 41007)
    return namespace


class KeywordRunnerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.run_dir = Path(self.temp.name) / 'test-call'
        self.run_dir.mkdir()
        self.module = data_module()

    def init(self, mode='keyword', require_gesture='0', profile='', target='127.0.0.1:50053', fps='', flags=(),
             gw_port='0', gesture_only='1', ai_target=''):
        args = ['external', ai_target, '', '', '', '6', '320', '240', '256', '0', '30', str(gw_port), '0',
                '1', '15', '2', '1', '300', 'external' if gesture_only == '1' else 'off', gesture_only, target, require_gesture,
                'source' if gesture_only == '1' else 'realtime', mode, str(profile), str(fps), ' '.join(flags)]
        self.module['data_main'](str(self.run_dir), 'init', *args)

    def load(self, name):
        return json.loads((self.run_dir / name).read_text())

    def test_keyword_config_and_single_leg_task_url(self):
        self.init()
        config = self.load('gateway.yaml')
        self.assertEqual(config['callback']['gesture_keywords']['mode'], 'keyword')
        self.assertEqual(list(config['video']['services']), ['video_effects'])
        self.assertEqual(config['video']['services']['video_effects']['ai']['task'], 'gesture_recognition')
        (self.run_dir / 'mf-events.jsonl').write_text(json.dumps({'event': 'ready', 'port': 9999}) + '\n')
        self.module['data_main'](str(self.run_dir), 'callback-ready')
        control = self.load('ctrl-result.json')['mediaResources']
        self.assertEqual(control['tCore']['callbackUrls']['gesture_recognition'],
                         'http://127.0.0.1:9999/nmf-mrm/v2/contexts/ctx-tAccess/termination/term-tAccess/keyword')
        self.assertEqual(control['tAccess']['callbackUrl'], 'http://127.0.0.1:9999/results/tAccess')

    def test_legacy_default_and_pixel_requirement(self):
        self.init('legacy', '1')
        self.assertNotIn('gesture_keywords', self.load('gateway.yaml')['callback'])
        with self.assertRaisesRegex(RuntimeError, 'receipt, not rendered stickers'):
            self.init('keyword', '1')

    def test_gesture_fps_default_override_and_expected_result_count(self):
        self.init()
        self.assertEqual(self.load('gateway.yaml')['video']['services']['video_effects']['gesture']['infer_fps'], 15)
        self.assertEqual(self.load('options.json')['minimum'], 54)
        self.init(fps='5')
        self.assertEqual(self.load('gateway.yaml')['video']['services']['video_effects']['gesture']['infer_fps'], 5)
        self.assertEqual(self.load('options.json')['minimum'], 18)
        for invalid in ('0', '61', '2.5'):
            with self.subTest(fps=invalid), self.assertRaises((ValueError, RuntimeError)):
                self.init(fps=invalid)

    def test_explicit_gesture_fps_overrides_only_effective_profile(self):
        path = self.profile()
        original = path.read_bytes()
        self.init(profile=path, target='', fps='5', flags=['--gesture-fps'])
        self.assertEqual(self.load('gateway.yaml')['video']['services']['video_effects']['gesture']['infer_fps'], 5)
        self.assertEqual(path.read_bytes(), original)
        self.assertEqual((self.run_dir / 'gateway-source.yaml').read_bytes(), original)
        self.assertEqual([c['path'] for c in self.load('gateway-config-overrides.json') if c['option'] != 'lab'],
                         ['video.services.video_effects.gesture.infer_fps'])

    def test_recv_failure_includes_grpc_cause_and_is_still_fatal(self):
        error = 'rpc error: code = DeadlineExceeded desc = MediaPipe gesture callback timed out'
        row = {'msg': 'ai: video recv error', 'session_id': self.run_dir.name + '-tcore',
               'ai_service': 'gesture_recognition', 'error': error}
        # A concurrently written last line must not hide the last complete error.
        (self.run_dir / 'gateway.log').write_text(json.dumps(row) + '\n{"msg":')
        baseline = {key: 0 for key in self.module['ERROR_METRICS']}
        current = dict(baseline)
        current['media_ai_video_ai_recv_errors_total'] = 1
        with self.assertRaisesRegex(RuntimeError, 'DeadlineExceeded.*MediaPipe gesture callback timed out'):
            self.module['check_errors'](current, baseline, allow_frame_eviction=True, run=self.run_dir)
        self.module['data_main'](str(self.run_dir), 'failure', 'AI receive failed')
        self.assertEqual(self.load('result.json')['ai_failure']['error'], error)

    def test_missing_ai_log_retains_original_metric_error(self):
        baseline = {key: 0 for key in self.module['ERROR_METRICS']}
        current = dict(baseline)
        current['media_ai_video_ai_recv_errors_total'] = 1
        with self.assertRaisesRegex(RuntimeError, 'ai_recv_errors_total increased by 1'):
            self.module['check_errors'](current, baseline, run=self.run_dir)

    def profile(self, mode='keyword'):
        config = {
            'server': {'http_addr': ':8443', 'public_url': 'https://10.10.1.5:8443',
                       'cert_file': '/etc/tls/gateway.crt', 'key_file': '/etc/tls/gateway.key'},
            'rtp': {'public_ip': '10.10.1.5', 'bind_ip': '10.10.1.5', 'port_start': 20000, 'port_end': 29999},
            'audio': {'enabled': False},
            'video': {'enabled': True,
                      'services': {'change_background': {'legs': 'tcore', 'segmentation': {'infer_width': 384, 'infer_height': 384}, 'ai': {'task': 'video_segment',
                                    'targets': ['segment.internal:50052']}},
                                   'video_effects': {'legs': 'tcore', 'gesture': {'infer_fps': 15}, 'ai': {'task': 'gesture_recognition',
                                    'targets': ['gesture-1.internal:50053', 'gesture-2.internal:50053']}}}},
            'callback': {'timeout_ms': 2000, 'max_retry': 3, 'gesture_keywords': {'mode': mode}},
            'result': {'dispatcher_workers': 32, 'queue_size': 16384, 'send_timeout_ms': 2000},
        }
        path = self.run_dir / 'ims.json'
        path.write_text(json.dumps(config, indent=3) + '\n')
        return path

    def test_ims_processing_defaults_are_preserved_with_local_lab_endpoints(self):
        path = self.profile()
        original = path.read_bytes()
        self.init(mode='', profile=path, target='')
        self.assertEqual((self.run_dir / 'gateway-source.yaml').read_bytes(), original)
        self.assertEqual(path.read_bytes(), original)
        options = self.load('options.json')
        self.assertEqual(options['gateway_config_sha256'], hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertNotEqual(options['gateway_config_effective_sha256'], options['gateway_config_sha256'])
        self.assertTrue(all(c['option'] == 'lab' for c in self.load('gateway-config-overrides.json')))
        self.assertEqual(options['gateway_config_source'], str(path.resolve()))
        self.assertEqual(options['gateway_base'], 'http://127.0.0.1:18081')
        self.assertEqual(options['rtp_ip'], '127.0.0.1')
        self.assertEqual(options['gesture_target'], '127.0.0.1:50053')
        self.assertEqual(options['callback_workers'], 32)
        self.assertEqual(options['callback_timeout_ms'], 2000)
        self.assertEqual(options['infer_size'], 384)
        self.assertEqual(options['minimum'], 54)  # 6 seconds × profile 15 FPS × 60%.
        self.assertEqual(options['gesture_callback_mode'], 'keyword')
        runtime = self.load('gateway.yaml')
        self.assertEqual(runtime['server']['cert_file'], '')
        self.assertEqual(runtime['server']['key_file'], '')
        self.assertEqual(runtime['server']['metrics_addr'], '')
        self.assertEqual(runtime['rtp']['bind_ip'], '127.0.0.1')
        self.assertEqual((runtime['rtp']['port_start'], runtime['rtp']['port_end']), (41000, 41007))
        self.assertEqual(runtime['callback']['url'], '')
        self.assertEqual(runtime['dcsf']['hosts'], [])
        self.assertFalse(runtime['pcap']['enabled'])
        self.assertGreaterEqual(runtime['session']['idle_timeout_sec'], 396)

    def test_explicit_target_and_mode_override_profile_defaults(self):
        path = self.profile('legacy')
        original = path.read_bytes()
        self.init(profile=path, flags=['--gesture-target', '--gesture-callback-mode'])
        config = self.load('gateway.yaml')
        self.assertEqual(config['video']['services']['video_effects']['ai']['targets'], ['127.0.0.1:50053'])
        self.assertEqual(config['callback']['gesture_keywords']['mode'], 'keyword')
        self.assertEqual(self.load('options.json')['gesture_target'], '127.0.0.1:50053')
        self.assertEqual(path.read_bytes(), original)

    def test_callback_and_inference_overrides_preserve_other_profile_fields(self):
        path = self.profile()
        self.init(profile=path, flags=['--callback-timeout', '--callback-workers', '--callback-retries', '--infer-size'])
        config = self.load('gateway.yaml')
        self.assertEqual(config['callback']['timeout_ms'], 15000)
        self.assertEqual(config['callback']['max_retry'], 1)
        self.assertEqual(config['result']['dispatcher_workers'], 2)
        self.assertEqual(config['result']['queue_size'], 16384)
        self.assertEqual(config['result']['send_timeout_ms'], 2000)
        self.assertEqual((config['video']['services']['change_background']['segmentation']['infer_width'],
                          config['video']['services']['change_background']['segmentation']['infer_height']), (256, 256))
        self.assertEqual(config['server']['cert_file'], '')
        self.assertEqual(config['rtp']['port_start'], 41000)
        options = self.load('options.json')
        self.assertNotEqual(options['gateway_config_effective_sha256'], options['gateway_config_sha256'])
        self.assertEqual(options['gateway_config_effective_sha256'],
                         hashlib.sha256((self.run_dir / 'gateway.yaml').read_bytes()).hexdigest())

    def test_positional_lab_defaults_do_not_override_profile_without_cli_flags(self):
        path = self.profile()
        self.init(mode='legacy', profile=path, target='127.0.0.1:50053', fps='5')
        config = self.load('gateway.yaml')
        self.assertEqual(config['video']['services']['video_effects']['gesture']['infer_fps'], 15)
        self.assertEqual(config['callback']['gesture_keywords']['mode'], 'keyword')
        self.assertEqual(config['callback']['timeout_ms'], 2000)
        self.assertEqual(config['callback']['max_retry'], 3)
        self.assertEqual(config['result']['dispatcher_workers'], 32)
        self.assertEqual(self.load('options.json')['infer_size'], 384)
        self.assertTrue(all(c['option'] == 'lab' for c in self.load('gateway-config-overrides.json')))

    def test_explicit_gateway_port_uses_local_http_without_tls(self):
        path = self.profile()
        self.init(profile=path, flags=['--gw-port'], gw_port='18443')
        config = self.load('gateway.yaml')
        self.assertEqual(config['server']['http_addr'], '127.0.0.1:18443')
        self.assertEqual(config['server']['public_url'], 'http://127.0.0.1:18443')
        self.assertEqual(config['server']['cert_file'], '')
        self.assertEqual(self.load('options.json')['gateway_base'], 'http://127.0.0.1:18443')

    def test_segmentation_target_override_keeps_gesture_on_local_default(self):
        path = self.profile()
        self.init(profile=path, gesture_only='0', ai_target='127.0.0.1:51052', flags=['--ai-target'])
        services = self.load('gateway.yaml')['video']['services']
        self.assertEqual(services['change_background']['ai']['targets'], ['127.0.0.1:51052'])
        self.assertEqual(services['video_effects']['ai']['targets'], ['127.0.0.1:50053'])
        self.assertEqual(self.load('options.json')['ai_target'], '127.0.0.1:51052')

    def test_segmentation_uses_local_worker_without_target_flag(self):
        self.init(mode='', profile=self.profile(), gesture_only='0', target='')
        self.assertEqual(self.load('options.json')['ai_target'], '127.0.0.1:50052')
        self.assertEqual(self.load('options.json')['profile_targets'], ['127.0.0.1:50052'])
        self.assertEqual(self.load('options.json')['minimum'], 18)

    def test_external_main_selects_ims_profile_without_gateway_config_flag(self):
        code = '''
source "$1"
video_data() { printf 'SELECTED_CONFIG=%s\\n' "$GATEWAY_CONFIG"; exit 0; }
main --ai-mode external --gesture-mode external --gesture-only --artifacts-dir "$2"
'''
        result = subprocess.run(['/bin/bash', '-c', code, '_', str(SCRIPT), str(self.run_dir)],
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('SELECTED_CONFIG=' + str(ROOT / 'config/gateway-video-ims.yaml'), result.stdout)

    def test_external_wrapper_does_not_inject_callback_timeout_default(self):
        fake_bin = self.run_dir / 'fake-bin'
        fake_bin.mkdir()
        bash = fake_bin / 'bash'
        bash.write_text('#!/bin/sh\nprintf "%s\\n" "$@"\n')
        bash.chmod(0o755)
        result = subprocess.run(['/bin/bash', str(ROOT / 'scripts/test-video-external-e2e.sh'), '--gesture-only'],
                                env={**os.environ, 'PATH': str(fake_bin) + ':' + os.environ['PATH']},
                                text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('--callback-timeout', result.stdout.splitlines())
        self.assertIn('--ai-mode', result.stdout.splitlines())

    def test_keyword_console_summarizes_gesture_changes_without_per_frame_logs(self):
        rows = [
            {'msg': 'keyword: before mapping', 'session_id': 's', 'frame_id': 1, 'response_id': 'r/1'},
            {'msg': 'keyword: after mapping', 'session_id': 's', 'frame_id': 1, 'response_id': 'r/1',
             'track_id': 1, 'gesture': 'Wave', 'topic_label': 'EMOTION', 'topic_info': 'congrat'},
            {'msg': 'keyword: before mapping', 'session_id': 's', 'frame_id': 3, 'response_id': 'r/3'},
            {'msg': 'keyword: after mapping', 'session_id': 's', 'frame_id': 3, 'response_id': 'r/3',
             'track_id': 1, 'gesture': 'Wave', 'topic_label': 'EMOTION', 'topic_info': 'congrat'},
            {'msg': 'keyword: skipped', 'gesture': 'Wave', 'reason': 'duplicate_event'},
            {'msg': 'keyword: delivered', 'topic_info': 'congrat'},
            {'msg': 'keyword: source details', 'gesture_result': {}},
        ]
        path = self.run_dir / 'gateway.log'
        path.write_text(''.join(json.dumps(row) + '\n' for row in rows))
        code = '''
source "$1"
RUN_DIR=$2
PIDS[gateway]=2147483647
stream_keyword_logs
wait "${PIDS[keyword-log]}"
unset 'PIDS[keyword-log]'
'''
        result = subprocess.run(['/bin/bash', '-c', code, '_', str(SCRIPT), str(self.run_dir)],
                                text=True, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [
            '[GESTURE] START session=s track=1 gesture=Wave frame=1 keyword=EMOTION/congrat',
            '[GESTURE] END session=s track=1 gesture=Wave frames=1..3 keyword=EMOTION/congrat',
        ])

    def test_gateway_ready_timeout_includes_curl_tls_error_and_url(self):
        code = '''
source "$1"
RUN_DIR=$2
GW_BASE=https://10.10.1.5:8443
check_services() { :; }
curl() { printf 'curl: (60) SSL certificate problem: self-signed certificate\\n' >&2; return 60; }
wait_for 'gateway ready' 0 health_ready
'''
        result = subprocess.run(['/bin/bash', '-c', code, '_', str(SCRIPT), str(self.run_dir)],
                                text=True, capture_output=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('https://10.10.1.5:8443/health/live', result.stderr)
        self.assertIn('SSL certificate problem: self-signed certificate', result.stderr)

    def test_ims_yaml_source_snapshot_is_exact_and_runtime_needs_no_cert(self):
        try:
            import yaml
        except ImportError:
            self.skipTest('YAML profile support requires PyYAML')
        path = ROOT / 'config/gateway-video-ims.yaml'
        source = yaml.safe_load(path.read_bytes())
        self.init(mode='', profile=path, target='')
        self.assertEqual((self.run_dir / 'gateway-source.yaml').read_bytes(), path.read_bytes())
        self.assertEqual(self.load('options.json')['rtp_ip'], '127.0.0.1')
        self.assertEqual(self.load('options.json')['profile_targets'], ['127.0.0.1:50053'])
        runtime = self.load('gateway.yaml')
        expected_video = source['video']
        for service, port in (('change_background', 50052), ('video_effects', 50053)):
            expected_video['services'][service]['ai']['targets'] = [f'127.0.0.1:{port}']
        self.assertEqual(runtime['video'], expected_video)
        self.assertEqual(runtime['result'], source['result'])
        self.assertEqual(runtime['pipeline'], source['pipeline'])
        self.assertEqual(runtime['callback']['timeout_ms'], source['callback']['timeout_ms'])
        self.assertEqual(runtime['callback']['gesture_keywords'], source['callback']['gesture_keywords'])
        self.assertEqual(runtime['server']['cert_file'], '')
        self.assertEqual(runtime['server']['key_file'], '')

    def test_signaling_checks_profile_rtp_ip_instead_of_loopback(self):
        state = {
            'answer_status': 201, 'ctrl_result_status': 200,
            'notify_event': {'selectedService': 'video_effects'}, 'callbacks': [],
            'answer': {'rtp_ip': '10.10.1.5', 'tcore_video_rtp_port': 20000,
                       'tcore_local_non_dc_video_media': {'sdpmLine': 'video 20000 RTP/AVP 96',
                                                        'sdpaLines': ['rtpmap:96 H264/90000']}},
        }
        args = [state, self.run_dir.name, 256, 2, 6, [['tcore', 'tAccess']], True, False, False]
        self.assertTrue(self.module['validate'](*args, '10.10.1.5')[0])
        with self.assertRaisesRegex(RuntimeError, 'RTP IP differs'):
            self.module['validate'](*args)

    def evidence(self):
        session = self.run_dir.name + '-tcore'
        keyword = {'topic_label': 'EMOTION', 'topic_info': 'congrat'}
        identity = {'session_id': session, 'response_id': 'recognizer/1'}
        key = 'a' * 64
        path = '/nmf-mrm/v2/contexts/ctx-tAccess/termination/term-tAccess/keyword'
        rows = [
            dict(identity, msg='keyword: before mapping', frame_id=1, rtp_timestamp=0, status='ok'),
            dict(identity, msg='keyword: sending', idempotency_key=key, **keyword,
                 hands=[{'raw_label': 'Open_Palm', 'raw_score': 0.9, 'gesture': 'Wave'}]),
            dict(identity, msg='keyword: http request', idempotency_key=key, method='POST',
                 url='http://mf' + path, body=json.dumps(keyword)),
            dict(identity, msg='keyword: http response', idempotency_key=key, status=200, protocol='HTTP/2.0'),
            dict(identity, msg='keyword: delivered', idempotency_key=key, **keyword),
            dict(identity, response_id='recognizer/2', msg='keyword: before mapping',
                 frame_id=151, rtp_timestamp=450000, status='ok'),
        ]
        receipts = [dict(keyword, event='keyword_received', idempotency_key=key,
                         contextId='ctx-tAccess', terminationId='term-tAccess', path=path, protocol='HTTP/2.0')]
        return rows, receipts

    def verify(self, rows, receipts):
        for name, values in [('gateway.log', rows), ('mf-events.jsonl', receipts)]:
            (self.run_dir / name).write_text(''.join(json.dumps(row) + '\n' for row in values))
        return self.module['keyword_evidence'](self.run_dir, 2, 6, [['tcore', 'tAccess']])

    def test_mapping_ack_receipt_and_dedup_snapshot_accounting(self):
        rows, receipts = self.evidence()
        complete, snapshots, keywords = self.verify(rows, receipts)
        self.assertTrue(complete)
        self.assertEqual(snapshots, {'tAccess': 2})
        self.assertEqual(keywords, {'tAccess': 1})

    def test_missing_ack_receipt_or_coverage_cannot_pass(self):
        for missing in ['keyword: http response', 'keyword: delivered', 'coverage', 'receipt']:
            with self.subTest(missing=missing):
                rows, receipts = self.evidence()
                if missing == 'receipt':
                    receipts = []
                elif missing == 'coverage':
                    rows[-1]['rtp_timestamp'] = 90000
                else:
                    rows = [row for row in rows if row['msg'] != missing]
                self.assertFalse(self.verify(rows, receipts)[0])

    def test_cross_leg_wrong_body_and_duplicate_activation_are_rejected(self):
        for corruption in ['session', 'resource', 'body', 'duplicate', 'retry']:
            with self.subTest(corruption=corruption):
                rows, receipts = self.evidence()
                if corruption == 'session':
                    rows[0]['session_id'] = self.run_dir.name + '-taccess'
                elif corruption == 'resource':
                    receipts[0]['contextId'] = 'ctx-tCore'
                elif corruption == 'body':
                    receipts[0]['topic_info'] = 'approve'
                elif corruption == 'duplicate':
                    receipts *= 2
                else:
                    rows.append(dict(rows[2], body=json.dumps({'topic_label': 'EMOTION', 'topic_info': 'approve'})))
                with self.assertRaises(RuntimeError):
                    self.verify(rows, receipts)


if __name__ == '__main__':
    unittest.main()
