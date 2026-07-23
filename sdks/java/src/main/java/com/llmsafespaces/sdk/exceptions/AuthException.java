package com.llmsafespaces.sdk.exceptions;

public class AuthException extends LLMSafeSpacesException {
    public AuthException(String message, int statusCode) {
        super(message, statusCode);
    }
}
