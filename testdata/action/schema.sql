CREATE TABLE users (
    id bigint PRIMARY KEY,
    email text NOT NULL
);

CREATE INDEX users_email_idx ON users (email);
