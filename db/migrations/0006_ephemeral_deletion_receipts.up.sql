-- A deletion confirmation is the only outbound row allowed after the user's
-- data purge. The sender physically removes it and its queue job immediately
-- after the delivery attempt reaches a terminal outcome.

ALTER TABLE outbound_messages
    ADD COLUMN ephemeral BOOLEAN NOT NULL DEFAULT false;
