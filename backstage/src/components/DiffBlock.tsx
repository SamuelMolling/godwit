import { makeStyles } from '@material-ui/core/styles';

const useStyles = makeStyles(theme => ({
  block: {
    margin: 0,
    padding: theme.spacing(1),
    overflowX: 'auto',
    fontFamily: 'monospace',
    fontSize: '0.8125rem',
    backgroundColor: theme.palette.background.default,
    border: `1px solid ${theme.palette.divider}`,
    borderRadius: theme.shape.borderRadius,
  },
  line: { display: 'block', whiteSpace: 'pre', paddingLeft: theme.spacing(0.5) },
  added: { color: theme.palette.success.main, borderLeft: `3px solid ${theme.palette.success.main}` },
  removed: { color: theme.palette.error.main, borderLeft: `3px solid ${theme.palette.error.main}` },
  changed: { color: theme.palette.warning.main, borderLeft: `3px solid ${theme.palette.warning.main}` },
  context: { borderLeft: '3px solid transparent' },
}));

type Kind = 'added' | 'removed' | 'changed' | 'context';

export function lineKind(line: string): Kind {
  switch (line[0]) {
    case '+':
      return 'added';
    case '-':
      return 'removed';
    case '~':
      return 'changed';
    default:
      return 'context';
  }
}

export const DiffBlock = ({ diff }: { diff: string }) => {
  const classes = useStyles();
  const lines = diff.split('\n').filter(l => l !== '');
  return (
    <pre className={classes.block}>
      {lines.map((line, i) => {
        const kind = lineKind(line);
        return (
          <code key={i} data-kind={kind} className={`${classes.line} ${classes[kind]}`}>
            {line}
          </code>
        );
      })}
    </pre>
  );
};
