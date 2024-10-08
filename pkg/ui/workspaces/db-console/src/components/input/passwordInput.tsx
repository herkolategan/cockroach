// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

import cn from "classnames";
import React from "react";

import EyeOff from "assets/eye-off.svg";
import Eye from "assets/eye.svg";

import { Button } from "../button";
import "./input.styl";

interface PasswordInputProps {
  onChange: (value: string) => void;
  value: string;
  placeholder?: string;
  className?: string;
  name?: string;
  label?: string;
}

interface PasswordInputState {
  showPassword?: boolean;
}

export class PasswordInput extends React.Component<
  PasswordInputProps,
  PasswordInputState
> {
  state = {
    showPassword: false,
  };

  handleOnTextChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    const value = event.target.value;
    this.props.onChange(value);
  };

  togglePassword = () => {
    this.setState({
      showPassword: !this.state.showPassword,
    });
  };

  renderPasswordIcon = (showPassword: boolean) => (
    <Button
      tabIndex={-1}
      type="flat"
      onClick={this.togglePassword}
      className="crl-button__show-password"
    >
      <img src={showPassword ? EyeOff : Eye} alt="Toggle Password" />
    </Button>
  );

  render() {
    const { placeholder, className, name, label, value } = this.props;
    const { showPassword } = this.state;
    const inputType = showPassword ? "text" : "password";

    const classes = cn(className, "crl-input", "crl-input__password");
    return (
      <div className="crl-input__wrapper">
        {label && (
          <label htmlFor={name} className="crl-input__label">
            {label}
          </label>
        )}
        <input
          name={name}
          type={inputType}
          value={value}
          placeholder={placeholder}
          className={classes}
          onChange={this.handleOnTextChange}
        />
        {this.renderPasswordIcon(showPassword)}
      </div>
    );
  }
}
