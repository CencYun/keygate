-- Back to "the owner plus max_seats members". Plans the up migration
-- raised (2 and over became 3 and over) go back; a plan at 2 made since
-- then is left as it is.
UPDATE plans SET max_seats = max_seats - 1 WHERE max_seats >= 3;
