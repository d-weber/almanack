// Event titles the smoke tests create through the editor.
//
// They live here rather than inline because two files need them: the specs that create
// them, and the clean-start check in auth.setup.js that looks for them left over from an
// earlier run. Put a title in one place and the check goes on passing over a database
// that is not clean, which is the failure this whole arrangement exists to prevent.
//
// Only the ones created through the UI are listed. A fixture created through the API is
// deleted by id in a finally, and has been since it was written; these two had nothing to
// delete by until they existed, which is how they came to be left behind.

export const MEETING = 'Test meeting';

export const HOSTILE_TITLE = '<img src=x onerror="window.__pwned = true">';

export const FIXTURE_TITLES = [MEETING, HOSTILE_TITLE];

/** Mutations carry the CSRF header or the middleware refuses them. */
export const HEADERS = { 'X-Requested-With': 'almanack' };
