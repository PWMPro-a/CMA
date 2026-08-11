import { useId } from 'react';
import { Select, type SelectOption } from './Select';

type SourceIpSelectProps = {
  value: string;
  options: ReadonlyArray<SelectOption>;
  onChange: (value: string) => void;
  label?: string;
  hint?: string;
  error?: string;
  disabled?: boolean;
  loading?: boolean;
  required?: boolean;
  className?: string;
  triggerClassName?: string;
  dropdownClassName?: string;
  ariaLabel?: string;
  ariaLabelledBy?: string;
  ariaDescribedBy?: string;
  id?: string;
};

export function SourceIpSelect({
  value,
  options,
  onChange,
  label,
  hint,
  error,
  disabled = false,
  loading = false,
  required = false,
  className,
  triggerClassName,
  dropdownClassName,
  ariaLabel,
  ariaLabelledBy,
  ariaDescribedBy,
  id,
}: SourceIpSelectProps) {
  const generatedId = useId();
  const selectId = id ?? generatedId;
  const select = (
    <Select
      id={selectId}
      value={value}
      options={options}
      onChange={onChange}
      disabled={disabled || loading}
      fullWidth
      ariaLabel={ariaLabel}
      ariaLabelledBy={ariaLabelledBy}
      ariaDescribedBy={ariaDescribedBy}
      className={className}
      triggerClassName={triggerClassName}
      dropdownClassName={dropdownClassName}
    />
  );

  if (!label && !hint && !error) {
    return select;
  }

  return (
    <div className="form-group">
      {label && <label htmlFor={selectId}>{label}{required ? ' *' : ''}</label>}
      {select}
      {hint && <div className="hint">{hint}</div>}
      {error && <div className="error-box">{error}</div>}
    </div>
  );
}
