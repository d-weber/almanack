// A reporter that lets one retry absorb a browser crash and nothing else.
//
// The suite runs with `retries: 1`, which on its own is the thing this project has said
// it does not want: a retry that turns any intermittent failure green hides exactly the
// flakes worth finding, and the suite has twice been bitten by a failure that pointed
// nowhere near its cause (#52, #66). But `chrome-headless-shell` segfaults mid-run (#80),
// and the test that reports it is the *next* one to ask for a context — an unrelated
// test, failed by a browser that died during its predecessor. Failing the run on that
// says nothing true about the code.
//
// So retries stay on, and this takes back everything they would otherwise hide: any test
// that failed and then passed is examined, and unless one of its failures was the crash,
// the run is failed again at the end and the test is named. A genuine intermittent bug
// therefore still turns CI red, with its own error printed, exactly as it would with no
// retries at all — it simply says so after the retry rather than before it.
//
// Playwright has no "retry only on this error" setting; a retry decision is made before
// the error can be inspected. Deciding afterwards is the same rule enforced at the only
// point the information exists.

// What a dead browser looks like from the test that trips over it. The first is what
// Playwright raises when the browser process is gone; the second is the signal the
// crashing process itself prints, which shows up when the crash lands inside the test
// rather than between two.
const CRASH_PATTERNS = [
  /Target (?:page|context|browser) (?:has been |was )?closed/i,
  /Target page, context or browser has been closed/i,
  /browser has been closed/i,
  /Browser closed unexpectedly/i,
  /SEGV_MAPERR|Received signal 11|browserType\.launch: Target closed/i,
];

function textOf(result) {
  const parts = [];
  for (const e of result.errors || []) {
    if (e.message) parts.push(e.message);
    if (e.stack) parts.push(e.stack);
    if (e.value) parts.push(String(e.value));
  }
  if (result.error && result.error.message) parts.push(result.error.message);
  return parts.join('\n');
}

function looksLikeACrash(result) {
  const text = textOf(result);
  return CRASH_PATTERNS.some((re) => re.test(text));
}

class CrashOnlyRetry {
  onBegin(_config, suite) {
    this.suite = suite;
  }

  onEnd(result) {
    // Nothing to take back from a run that already failed, or from one whose tests were
    // never retried.
    if (!this.suite || result.status !== 'passed') return;

    const hidden = [];
    for (const test of this.suite.allTests()) {
      if (test.outcome() !== 'flaky') continue;
      const failures = (test.results || []).filter(
        (r) => r.status === 'failed' || r.status === 'timedOut',
      );
      if (failures.length && failures.some(looksLikeACrash)) continue; // the crash: allowed
      hidden.push({ title: test.titlePath().filter(Boolean).join(' › '), failures });
    }
    if (!hidden.length) return;

    console.error(
      `\n${hidden.length} test(s) passed only on a retry, for something other than the ` +
        `browser crash retries exist for (#80). Failing the run: a retry that hides an ` +
        `intermittent bug is worse than no retry at all.\n`,
    );
    for (const { title, failures } of hidden) {
      console.error(`  ✗ ${title}`);
      const first = failures[0];
      if (first) {
        const line = textOf(first).split('\n').find((l) => l.trim()) || '(no message)';
        console.error(`      first attempt: ${line.trim()}`);
      }
    }
    console.error(
      `\nIf this is a new browser-level crash rather than a real flake, add its signature ` +
        `to CRASH_PATTERNS in e2e/crash-only-retry.js and say why in the commit.\n`,
    );
    return { status: 'failed' };
  }
}

export default CrashOnlyRetry;
