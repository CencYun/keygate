-- max_seats now counts the owner: it is how many people a license
-- holds, as the dashboard, the portal and the docs always described it.
-- The server used to allow the owner plus max_seats members, so plans
-- that allowed more than one person keep their capacity: 5 becomes 6.
-- A plan at 1 stays at 1. The portal showed it as a single seat and
-- offered no invites, and now the server agrees. 0 still means no limit.
UPDATE plans SET max_seats = max_seats + 1 WHERE max_seats >= 2;
