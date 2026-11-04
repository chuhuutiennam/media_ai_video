"""Exercise the deployed .sh entry points using an isolated fake TCP worker."""
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPTS=Path(__file__).resolve().parents[1]/'scripts'
BASE_PYTHON=str(Path(getattr(sys, '_base_executable', sys.executable)).resolve())


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(prefix='gesture shell ')
        self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name)
        self.app=self.root/'app'; self.venv=self.root/'venv'; self.source=self.root/'source'
        for name in ('scripts','etc','models','run','logs'):(self.app/name).mkdir(parents=True)
        for p in SCRIPTS.glob('*.sh'):shutil.copy2(p,self.app/'scripts'/p.name)
        (self.venv/'bin').mkdir(parents=True)
        self.pip_log=self.root/'pip.log'
        (self.venv/'bin/python').write_text('''#!/usr/bin/env bash
if [[ "$1" == -m && "$2" == pip ]]; then printf '%s\\n' "$*" >> "$TEST_PIP_LOG"; exit 0; fi
exec "$TEST_PYTHON" "$@"
''')
        (self.venv/'bin/mediapipe-gesture-server').write_text('''#!/usr/bin/env bash
exec "$TEST_PYTHON" -m mediapipe_gesture.server "$@"
''')
        for file in (self.venv/'bin').iterdir():file.chmod(0o755)
        (self.app/'.app_venv').write_text(str(self.venv)+'\n')
        self.model=self.app/'models/gesture_recognizer.task';self.model.write_bytes(b'fake')
        package=self.root/'mediapipe_gesture';package.mkdir()
        (package/'__init__.py').write_text('')
        (package/'config.py').write_text('''import json
from pathlib import Path
from types import SimpleNamespace as NS
def load_config(path):
 data=json.loads(path.read_text())
 return NS(gesture=NS(enabled=True,model=Path(data['model']) if data['model'] else None,delegate='cpu',max_concurrent_recognizers=4),server=NS(addr=data['addr'],max_streams=8),metrics=NS(enabled=data.get('metrics',0)>0,port=data.get('metrics',0)),logging=NS(file=None))
''')
        (package/'server.py').write_text('''import argparse,json,os,signal,socket,sys,time
p=argparse.ArgumentParser();p.add_argument('--config');args=p.parse_args()
data=json.load(open(args.config));mode=os.environ.get('TEST_MODE')
if mode=='fail':print('model initialization failed',flush=True);sys.exit(8)
if mode=='stall':time.sleep(60)
if mode=='ignore-term':signal.signal(signal.SIGTERM,lambda *_:None)
host,port=data['addr'].rsplit(':',1)
with socket.socket() as s:
 s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
 s.bind((host,int(port)));s.listen()
 while True:
  c,_=s.accept();c.close()
''')
        for name in ('grpc','grpc_tools','mediapipe','cv2','numpy','prometheus_client','google','google/protobuf'):
            folder=self.root/name;folder.mkdir(exist_ok=True);(folder/'__init__.py').write_text('')
        with socket.socket() as s:s.bind(('127.0.0.1',0));self.port=s.getsockname()[1]
        self.config=self.app/'etc/config.yaml'
        self.data=dict(addr=f'127.0.0.1:{self.port}',model=str(self.model))
        self.write_config()
        self.env={**os.environ,'PYTHONPATH':str(self.root),'PYTHONDONTWRITEBYTECODE':'1',
                  'PYTHON':BASE_PYTHON,'TEST_PYTHON':BASE_PYTHON,'TEST_PIP_LOG':str(self.pip_log)}
        (self.source/'scripts').mkdir(parents=True)
        for p in SCRIPTS.glob('*.sh'):shutil.copy2(p,self.source/'scripts'/p.name)
        (self.source/'pyproject.toml').write_text('[project]\nname="fixture"\n')
        (self.source/'etc').mkdir();(self.source/'etc/config.yaml').write_text('')
        (self.source/'src/proto').mkdir(parents=True);(self.source/'src/proto/video.proto').write_text('syntax="proto3";')
        (self.source/'checkpoint').mkdir();(self.source/'checkpoint/gesture_recognizer.task').write_bytes(b'new fake')
        self.addCleanup(self.cleanup_worker)

    def write_config(self):self.config.write_text(json.dumps(self.data))
    def cli(self, action, *args, expected=0, env=None):
        result=subprocess.run(['bash',str(self.app/'scripts'/f'{action}.sh'),'--app-dir',str(self.app),*map(str,args)],
                              env={**self.env,**(env or {})},cwd='/tmp',capture_output=True,text=True,timeout=40)
        self.assertEqual(result.returncode,expected,result.stdout+result.stderr)
        output=result.stdout+result.stderr
        self.assertNotIn(f'[{action}.sh]',output)
        return output
    def pid(self):return int((self.app/'run/mediapipe-gesture.pid').read_text())
    def cleanup_worker(self):
        subprocess.run(['bash',str(self.app/'scripts/stop.sh'),'--app-dir',str(self.app),'--venv',str(self.venv),'--grace','1','--force'],
                       env=self.env,cwd='/tmp',capture_output=True,timeout=15)

    def test_start_status_idempotence_restart_stop(self):
        self.assertFalse((self.app/'scripts/_common.sh').exists())
        self.assertIn('Ready PID=',self.cli('start','--timeout','3'))
        first=self.pid();self.assertIn('Already running',self.cli('start'))
        self.assertEqual(self.pid(),first);self.assertIn('Running PID=',self.cli('status'))
        self.cli('restart','--timeout','3');self.assertNotEqual(self.pid(),first)
        self.cli('stop');self.cli('status',expected=3)
        self.assertFalse((self.app/'run/mediapipe-gesture.json').exists())
        self.assertIn('Sending SIGTERM',(self.app/'logs/service.log').read_text())
    def test_stale_pid_does_not_signal_another_process(self):
        owner=subprocess.Popen([BASE_PYTHON,'-c','import time;time.sleep(60)'])
        try:
            (self.app/'run/mediapipe-gesture.pid').write_text(str(owner.pid)+'\n')
            (self.app/'run/mediapipe-gesture.start').write_text('0\n')
            (self.app/'run/mediapipe-gesture.addr').write_text(self.data['addr']+'\n')
            self.cli('stop');self.assertIsNone(owner.poll())
        finally:owner.terminate();owner.wait()
    def test_legacy_json_metadata_remains_manageable(self):
        self.cli('start');(self.app/'run/mediapipe-gesture.start').unlink();(self.app/'run/mediapipe-gesture.addr').unlink()
        self.cli('status');self.cli('stop');self.cli('status',expected=3)
    def test_occupied_port_refuses_start(self):
        with socket.socket() as owner:
            owner.bind(('127.0.0.1',self.port));owner.listen()
            self.assertIn('port occupied',self.cli('start',expected=1))
        self.assertFalse((self.app/'run/mediapipe-gesture.pid').exists())
    def test_conflicting_metrics_port_refuses_start(self):
        self.data['metrics']=self.port;self.write_config()
        self.assertIn('conflicting metrics',self.cli('start',expected=1))
    def test_missing_model_reports_config_guidance(self):
        self.data['model']=None;self.write_config()
        self.assertIn('gesture.model',self.cli('start',expected=1))
    def test_worker_failure_cleans_pid_and_shows_log(self):
        self.assertIn('model initialization failed',self.cli('start','--timeout','3',expected=1,env={'TEST_MODE':'fail'}))
        self.assertFalse((self.app/'run/mediapipe-gesture.pid').exists())
    def test_startup_timeout_stops_child(self):
        self.assertIn('Startup timeout',self.cli('start','--timeout','1',expected=1,env={'TEST_MODE':'stall'}))
        self.assertFalse((self.app/'run/mediapipe-gesture.pid').exists())
    def test_shutdown_timeout_retains_state_until_force(self):
        self.cli('start','--timeout','3',env={'TEST_MODE':'ignore-term'})
        self.assertIn('metadata retained',self.cli('stop','--grace','1',expected=1))
        self.assertTrue((self.app/'run/mediapipe-gesture.pid').exists())
        self.cli('stop','--grace','1','--force')
        self.assertFalse((self.app/'run/mediapipe-gesture.pid').exists())
    def test_stop_works_without_worker_venv(self):
        self.cli('start');shutil.rmtree(self.venv);self.cli('stop')
    def test_empty_marker_requires_explicit_venv(self):
        (self.app/'.app_venv').write_text('')
        self.assertIn('Empty marker',self.cli('start',expected=1))
        self.cli('start','--venv',self.venv,'--timeout','3')
        (self.app/'.app_venv').write_text(str(self.venv)+'\n')
    def test_install_dry_run_is_read_only(self):
        target=self.root/'not installed'
        result=subprocess.run(['bash',str(self.source/'scripts/install.sh'),'--app-dir',str(target),'--dry-run'],
                              env=self.env,cwd='/tmp',capture_output=True,text=True,timeout=10)
        self.assertEqual(result.returncode,0,result.stdout+result.stderr)
        self.assertFalse(target.exists());self.assertFalse(self.pip_log.exists())
        self.assertIn('DRY-RUN',result.stdout)
    def test_upgrade_preserves_config_model_and_restarts(self):
        config=self.config.read_bytes();model=self.model.read_bytes()
        (self.app/'scripts/manage.py').write_text('obsolete helper')
        (self.app/'scripts/_common.sh').write_text('obsolete shell helper')
        self.cli('start');old=self.pid()
        self.cli('upgrade','--src-dir',self.source,'--timeout','3')
        self.assertNotEqual(old,self.pid());self.cli('status')
        self.assertEqual(self.config.read_bytes(),config);self.assertEqual(self.model.read_bytes(),model)
        self.assertFalse((self.app/'scripts/manage.py').exists())
        self.assertFalse((self.app/'scripts/_common.sh').exists())
        self.assertIn('--upgrade',self.pip_log.read_text())
    def test_upgrade_no_restart_and_offline_flags(self):
        self.cli('start');wheels=self.root/'wheels';wheels.mkdir()
        self.cli('upgrade','--src-dir',self.source,'--offline','--wheels-dir',wheels,'--no-restart')
        self.cli('status',expected=3)
        self.assertIn('--no-index',self.pip_log.read_text());self.assertIn('--no-build-isolation',self.pip_log.read_text())
    def test_upgrade_dry_run_does_not_stop_process(self):
        self.cli('start');pid=self.pid()
        self.cli('upgrade','--src-dir',self.source,'--dry-run')
        self.assertEqual(self.pid(),pid);self.cli('status');self.assertFalse(self.pip_log.exists())


if __name__=='__main__':unittest.main()
