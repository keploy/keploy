# Some sample test cases for Boundary Value Testing:

## TestCase1- Minimum email and password length
```
email: "e"
password: "p"
```
This test case will test if the API is able to handle the minimum allowed email and password lengths, and whether it returns a JWT token if the credentials are valid.


## TestCase2- Maximum email length and empty password
```
email: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"
password: ""
```
This test case will test if the API is able to handle the maximum allowed email length and an empty password, and whether it returns a JWT token if the credentials are valid.

## TestCase3- Empty email and typical password
```
email: ""
password: "password123"
```
This test case will test if the API is able to handle an empty email field and a typical password value, and whether it returns a JWT token if the credentials are valid.

## TestCase4- Maximum email length + 1 and password length just above maximum
```
email: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz1"
password: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz1"
```
This test case will test if the API is able to handle an email length just above the maximum allowed length and a password length just above the maximum allowed length, and whether it returns an error if the credentials are invalid.

## TestCase5- Typical email and maximum password length
```
email: "user@example.com"
password: "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"
```
This test case will test if the API is able to handle a typical email and the maximum allowed password length, and whether it returns a JWT token if the credentials are valid.
