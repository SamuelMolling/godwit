CREATE TABLE public.people (
    id bigserial PRIMARY KEY,
    age integer NOT NULL
);

INSERT INTO public.people (age) SELECT g FROM generate_series(1, 2000) g;
