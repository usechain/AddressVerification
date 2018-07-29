# AddressVerification
The verification process introduce and data struct

Usechain的身份验证主要包括四个主要的过程：
（1）首先， 用户需要到权威的第三方CA认证机构进行身份证书申请；

（2）然后， 用户可以通过usechain节点客户端程序或者钱包产生一个一次性地址的密钥对，绑定身份证书进行身份验证；

（3）再次， 用户绑定一次性地址成功后，以一次性地址作为父地址，通过AB账户模型产生主地址。每个用户有且仅用一个主地址；用环签名和非对称加密保证主地址和一次性地址关系不可见，以AB账户模型保证身份关系必要时可检索；

（4）最后， 用户可以以主地址或者子地址为父地址，产生一系列的子地址，每个用户可以拥有无限多的子地址。认证过程大体类似主地址认证


![avatar](https://github.com/usechain/AddressVerification/blob/master/process.png)
