/** A widget class. */
export class Widget {
  name: string;

  /** Draws the widget. */
  render(): string {
    return brackets(this.name);
  }
}

// brackets wraps s in square brackets.
function brackets(s: string): string {
  return `[${s}]`;
}

export const MAX = 10;
