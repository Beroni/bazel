import fs from 'node:fs';

/**
 * takeLock makes `file` this run's, and returns what gives it back. Two runs on
 * one output directory delete each other's servers and swap .out/bin/bazel, so
 * a result could be about the wrong code: the second one stops here instead.
 */
export function takeLock(file: string): () => void {
  for (;;) {
    try {
      fs.writeFileSync(file, String(process.pid), { flag: 'wx' });
      return () => fs.rmSync(file, { force: true });
    } catch (err: any) {
      if (err.code !== 'EEXIST') throw err;
    }
    const pid = Number(fs.readFileSync(file, 'utf8'));
    if (alive(pid)) {
      throw new Error(`another e2e run (pid ${pid}) is using this output directory: wait for it, or give this run its own with E2E_OUT=<dir>`);
    }
    fs.rmSync(file, { force: true }); // left by a run that died
  }
}

function alive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (err: any) {
    return err.code === 'EPERM';
  }
}
