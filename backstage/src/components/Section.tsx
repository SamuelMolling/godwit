import { ReactNode } from 'react';
import { Typography } from '@material-ui/core';

export const Section = ({ title, children }: { title: string; children: ReactNode }) => (
  <section>
    <Typography variant="h6" component="h3" gutterBottom>
      {title}
    </Typography>
    {children}
  </section>
);

export const Unavailable = ({ what, error }: { what: string; error: Error }) => (
  <Typography role="alert" color="error" variant="body2">
    Could not read {what}: {error.message}
  </Typography>
);
