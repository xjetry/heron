export function MixedCheckbox({ label, checked, disabled, onChange }: {
  label: string; checked: boolean | "mixed"; disabled?: boolean; onChange: () => void;
}) {
  return <input type="checkbox" aria-label={label} aria-checked={checked} checked={checked === true} disabled={disabled}
    ref={(input) => { if (input) input.indeterminate = checked === "mixed"; }} onChange={onChange} />;
}
