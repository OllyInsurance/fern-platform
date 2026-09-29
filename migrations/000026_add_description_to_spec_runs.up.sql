-- What the test DOES, in plain English, separate from what went wrong.
--
-- The reporter had nowhere to put this, so a Playwright suite folded its
-- test.step walk into error_message. That column is only meaningful on a
-- failure — and the platform drops it for passing specs — so the description
-- of a green test was written, stored and then silently discarded, which is
-- exactly the run where a reviewer most needs to know what the video shows.
ALTER TABLE spec_runs ADD COLUMN description TEXT;
